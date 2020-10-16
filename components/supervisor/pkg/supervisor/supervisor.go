// Copyright (c) 2020 TypeFox GmbH. All rights reserved.
// Licensed under the GNU Affero General Public License (AGPL).
// See License-AGPL.txt in the project root for license information.

package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gitpod-io/gitpod/common-go/log"
	csapi "github.com/gitpod-io/gitpod/content-service/api"
	"github.com/gitpod-io/gitpod/content-service/pkg/executor"
	"github.com/gitpod-io/gitpod/content-service/pkg/initializer"
	"github.com/gitpod-io/gitpod/supervisor/api"
	"github.com/gitpod-io/gitpod/supervisor/pkg/dropwriter"
	"github.com/gitpod-io/gitpod/supervisor/pkg/gitpod"
	"github.com/gitpod-io/gitpod/supervisor/pkg/ports"
	"github.com/gitpod-io/gitpod/supervisor/pkg/terminal"
	daemon "github.com/gitpod-io/gitpod/ws-daemon/api"

	"github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/soheilhy/cmux"
	"google.golang.org/grpc"
)

var (
	additionalServices []RegisterableService
	apiEndpointOpts    []grpc.ServerOption
)

// RegisterAdditionalService registers additional services for the API endpoint
// of supervisor.
func RegisterAdditionalService(services ...RegisterableService) {
	additionalServices = append(additionalServices, services...)
}

// AddAPIEndpointOpts adds additional grpc server options for the API endpoint
func AddAPIEndpointOpts(opts ...grpc.ServerOption) {
	apiEndpointOpts = append(apiEndpointOpts, opts...)
}

const (
	maxIDEPause = 20 * time.Second
)

type runOptions struct {
	Args                  []string
	WithoutTeardownCanary bool
}

// RunOption customizes the run behaviour
type RunOption func(*runOptions)

// WithArgs sets the arguments passed to Run
func WithArgs(args []string) RunOption {
	return func(r *runOptions) {
		r.Args = args
	}
}

// WithoutTeardownCanary prevents supervisor from triggering the teardown canary
func WithoutTeardownCanary() RunOption {
	return func(ro *runOptions) {
		ro.WithoutTeardownCanary = true
	}
}

// Run serves as main entrypoint to the supervisor
func Run(options ...RunOption) {
	opts := runOptions{
		Args: os.Args,
	}
	for _, o := range options {
		o(&opts)
	}

	cfg, err := GetConfig()
	if err != nil {
		log.WithError(err).Fatal("configuration error")
	}
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Println("supervisor makes sure your workspace/IDE keeps running smoothly.\nYou don't have to call this thing, Gitpod calls it for you.")
		return
	}

	buildIDEEnv(&Config{})
	configureGit(cfg)

	tokenService := NewInMemoryTokenService()
	tkns, err := cfg.GetTokens(true)
	if err != nil {
		log.WithError(err).Warn("cannot prepare tokens")
	}
	for _, tks := range tkns {
		_, err = tokenService.SetToken(context.Background(), &tks.SetTokenRequest)
		if err != nil {
			log.WithError(err).Warn("cannot prepare tokens")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	var (
		shutdown            = make(chan struct{})
		ideReady            = make(chan struct{})
		cstate              = NewInMemoryContentState(cfg.RepoRoot)
		gitpodService       = createGitpodService(cfg, tokenService)
		gitpodConfigService = gitpod.NewConfigService(cfg.RepoRoot+"/.gitpod.yml", cstate.ContentReady())
		portMgmt            = ports.NewManager(
			createExposedPortsImpl(cfg, gitpodService),
			&ports.PollingServedPortsObserver{
				RefreshInterval: 2 * time.Second,
			},
			ports.NewConfigService(cfg.WorkspaceID, gitpodConfigService, gitpodService),
			uint32(cfg.IDEPort),
			uint32(cfg.APIEndpointPort),
		)
		termMux    = terminal.NewMux()
		termMuxSrv = terminal.NewMuxTerminalService(termMux)
	)
	taskManager := newTasksManager(cfg, termMuxSrv, cstate)

	termMuxSrv.DefaultWorkdir = cfg.RepoRoot

	apiServices := []RegisterableService{
		&statusService{
			ContentState: cstate,
			Ports:        portMgmt,
			Tasks:        taskManager,
			IDEReady:     ideReady,
		},
		termMuxSrv,
		RegistrableTokenService{tokenService},
		&InfoService{cfg: cfg},
		&ControlService{portsManager: portMgmt},
	}
	apiServices = append(apiServices, additionalServices...)

	var wg sync.WaitGroup
	wg.Add(5)
	go startAndWatchIDE(ctx, cfg, &wg, ideReady)
	go startContentInit(ctx, cfg, &wg, cstate)
	go startAPIEndpoint(ctx, cfg, &wg, apiServices, apiEndpointOpts...)
	go taskManager.Run(ctx, &wg)
	go func() {
		defer wg.Done()
		portMgmt.Run()
	}()

	if cfg.PreventMetadataAccess {
		go func() {
			if !hasMetadataAccess() {
				return
			}

			log.Error("metadata access is possible - shutting down")
			close(shutdown)
		}()
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sigChan:
	case <-shutdown:
	}

	log.Info("received SIGTERM - tearing down")
	teardown(!opts.WithoutTeardownCanary)

	cancel()
	wg.Wait()
}

func createGitpodService(cfg *Config, tknsrv api.TokenServiceServer) *gitpod.APIoverJSONRPC {
	endpoint, host, err := cfg.GitpodAPIEndpoint()
	if err != nil {
		log.WithError(err).Fatal("cannot find Gitpod API endpoint")
		return nil
	}
	tknres, err := tknsrv.GetToken(context.Background(), &api.GetTokenRequest{
		Host: host,
		Scope: []string{
			"function:openPort",
			"function:getOpenPorts",
		},
	})
	if err != nil {
		log.WithError(err).Error("cannot get token for Gitpod API")
		return nil
	}

	gitpodService, err := gitpod.ConnectToServer(endpoint, gitpod.ConnectToServerOpts{
		Token: tknres.Token,
	})
	if err != nil {
		log.WithError(err).Error("cannot connect to Gitpod API")
		return nil
	}
	return gitpodService
}

func createExposedPortsImpl(cfg *Config, gitpodService *gitpod.APIoverJSONRPC) (res ports.ExposedPortsInterface) {
	if gitpodService == nil {
		log.Error("auto-port exposure won't work")
		return &ports.NoopExposedPorts{}
	}

	return &ports.GitpodExposedPorts{
		WorkspaceID: cfg.WorkspaceID,
		C:           gitpodService,
	}
}

func configureGit(cfg *Config) {
	settings := [][]string{
		{"push.default", "simple"},
		{"alias.lg", "log --color --graph --pretty=format:'%Cred%h%Creset -%C(yellow)%d%Creset %s %Cgreen(%cr) %C(bold blue)<%an>%Creset' --abbrev-commit"},
		{"credential.helper", "/usr/bin/gp credential-helper"},
	}
	if cfg.GitUsername != "" {
		settings = append(settings, []string{"user.name", cfg.GitUsername})
	}
	if cfg.GitEmail != "" {
		settings = append(settings, []string{"user.email", cfg.GitEmail})
	}

	for _, s := range settings {
		cmd := exec.Command("git", append([]string{"config", "--global"}, s...)...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err != nil {
			log.WithError(err).WithField("args", s).Warn("git config error")
		}
	}
}

func hasMetadataAccess() bool {
	// curl --connect-timeout 10 -s -H "Metadata-Flavor: Google" 'http://169.254.169.254/computeMetadata/v1/instance/'
	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	req, err := http.NewRequest("GET", "http://169.254.169.254/computeMetadata/v1/instance/", nil)
	if err != nil {
		log.WithError(err).Error("cannot check metadata access - this should never happen")
		return true
	}
	req.Header.Add("Metadata-Flavor", "Google")

	resp, err := client.Do(req)
	// We did not see an error. That's a problem becuase that means that users can reach the metadata endpoint.
	if err == nil {
		resp.Body.Close()
		return true
	}

	// if we see any error here we're good because then the request timed out or failed for some other reason.
	return false
}

func startAndWatchIDE(ctx context.Context, cfg *Config, wg *sync.WaitGroup, ideReady chan<- struct{}) {
	defer wg.Done()

	type status int
	const (
		statusNeverRan status = iota
		statusShouldRun
		statusShouldShutdown
	)
	s := statusNeverRan

	var (
		cmd        *exec.Cmd
		ideStopped chan struct{}
	)
supervisorLoop:
	for {
		if s != statusShouldShutdown {
			ideStopped = make(chan struct{}, 1)

			cmd = prepareIDELaunch(cfg)
			err := cmd.Start()
			if err != nil {
				if s == statusNeverRan {
					log.WithError(err).Fatal("IDE failed to start")
				}

				continue
			}
			s = statusShouldRun

			go runIDEReadinessProbe(cfg, ideReady)

			go func() {
				err := cmd.Wait()
				if err != nil && !strings.Contains(err.Error(), "signal: interrupt") {
					log.WithError(err).Warn("IDE was stopped")
				}

				close(ideStopped)
			}()
		}

		select {
		case <-ideStopped:
			// IDE was stopped - let's just restart it after a small delay (in case the IDE doesn't start at all) in the next round
			if s == statusShouldShutdown {
				break supervisorLoop
			}
			time.Sleep(1 * time.Second)
		case <-ctx.Done():
			// we've been asked to shut down
			s = statusShouldShutdown
			cmd.Process.Signal(os.Interrupt)
			break supervisorLoop
		}
	}

	log.Info("IDE supervisor loop ended - waiting for IDE to come down")
	select {
	case <-ideStopped:
		return
	case <-time.After(30 * time.Second):
		log.Fatal("IDE did not stop after 30 seconds")
	}
}

func prepareIDELaunch(cfg *Config) *exec.Cmd {
	var args []string
	args = append(args, cfg.WorkspaceRoot)
	args = append(args, "--port", strconv.Itoa(cfg.IDEPort))
	args = append(args, "--hostname", "0.0.0.0")
	log.WithField("args", args).WithField("entrypoint", cfg.Entrypoint).Info("launching IDE")

	cmd := exec.Command(cfg.Entrypoint, args...)
	cmd.Env = buildIDEEnv(cfg)

	// We need the IDE to run in its own process group, s.t. we can suspend and resume
	// IDE and its children.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Here we must resist the temptation to "neaten up" the IDE output for headless builds.
	// This would break the JSON parsing of the headless builds.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if lrr := cfg.LogRateLimit(); lrr > 0 {
		limit := int64(lrr)
		cmd.Stdout = dropwriter.Writer(cmd.Stdout, dropwriter.NewBucket(limit*1024*3, limit*1024))
		cmd.Stderr = dropwriter.Writer(cmd.Stderr, dropwriter.NewBucket(limit*1024*3, limit*1024))
		log.WithField("limit_kb_per_sec", limit).Info("rate limiting IDE log output")
	}

	return cmd
}

func buildIDEEnv(cfg *Config) []string {
	var env, envn []string
	for _, e := range os.Environ() {
		segs := strings.Split(e, "=")
		if len(segs) < 2 {
			log.Printf("\"%s\" has invalid format, not including in IDE environment", e)
			continue
		}
		nme := segs[0]

		if isBlacklistedEnvvar(nme) {
			continue
		}

		env = append(env, e)
		envn = append(envn, nme)
	}

	ce := map[string]string{
		"SUPERVISOR_ADDR": fmt.Sprintf("localhost:%d", cfg.APIEndpointPort),
	}
	for nme, val := range ce {
		log.WithField("envvar", nme).Debug("passing environment variable to IDE")
		env = append(env, fmt.Sprintf("%s=%s", nme, val))
		envn = append(envn, nme)
	}

	log.WithField("envvar", envn).Debug("passing environment variables to IDE")

	return env
}

func runIDEReadinessProbe(cfg *Config, ideReady chan<- struct{}) {
	defer close(ideReady)
	defer log.Info("IDE is ready")

	switch cfg.ReadinessProbe.Type {
	case ReadinessProcessProbe:
		return

	case ReadinessHTTPProbe:
		var (
			url    = fmt.Sprintf("http://localhost:%d/%s", cfg.IDEPort, strings.TrimPrefix(cfg.ReadinessProbe.HTTPProbe.Path, "/"))
			client = http.Client{Timeout: 5 * time.Second}
			tick   = time.NewTicker(5 * time.Second)
		)
		defer tick.Stop()
		for {
			resp, err := client.Get(url)
			if err != nil {
				log.WithError(err).Info("IDE is not ready yet")
			} else if resp.StatusCode != http.StatusOK {
				log.WithField("status", resp.StatusCode).Info("IDE readiness probe came back with non-200 status code")
			} else {
				break
			}

			<-tick.C
		}
	}
}

func isBlacklistedEnvvar(name string) bool {
	// exclude blacklisted
	prefixBlacklist := []string{
		"THEIA_SUPERVISOR_",
		"GITPOD_TOKENS",
		// The following vars are meant to filter out the kubernetes-injected env vars that we do not know how to turn of (yet)
		"KUBERNETES_SERVICE",
		"KUBERNETES_PORT",
		// This is a magic env var is set to /theia/supervisor. We do not want to point users at it.
		"   ", // 3 spaces
	}
	for _, wep := range prefixBlacklist {
		if strings.HasPrefix(name, wep) {
			return true
		}
	}

	return false
}

func startAPIEndpoint(ctx context.Context, cfg *Config, wg *sync.WaitGroup, services []RegisterableService, opts ...grpc.ServerOption) {
	defer wg.Done()

	l, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.APIEndpointPort))
	if err != nil {
		log.WithError(err).Fatal("cannot start health endpoint")
	}

	m := cmux.New(l)
	restMux := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{EnumsAsInts: false, EmitDefaults: true}),
	)
	grpcMux := m.MatchWithWriters(cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))
	grpcServer := grpc.NewServer(opts...)
	grpcEndpoint := fmt.Sprintf("localhost:%d", cfg.APIEndpointPort)
	for _, reg := range services {
		if reg, ok := reg.(RegisterableGRPCService); ok {
			reg.RegisterGRPC(grpcServer)
		}
		if reg, ok := reg.(RegisterableRESTService); ok {
			err := reg.RegisterREST(restMux, grpcEndpoint)
			if err != nil {
				log.WithError(err).Fatal("cannot register REST service")
			}
		}
	}
	go grpcServer.Serve(grpcMux)

	httpMux := m.Match(cmux.HTTP1Fast())
	routes := http.NewServeMux()
	routes.Handle("/_supervisor/v1/", http.StripPrefix("/_supervisor", restMux))
	routes.Handle("/_supervisor/frontend", http.FileServer(http.Dir(cfg.FrontendLocation)))
	go http.Serve(httpMux, routes)

	go m.Serve()

	<-ctx.Done()
	log.Info("shutting down API endpoint")
	l.Close()
}

func startContentInit(ctx context.Context, cfg *Config, wg *sync.WaitGroup, cst ContentState) {
	defer wg.Done()
	defer log.Info("supervisor: workspace content available")

	var err error
	defer func() {
		if err == nil {
			return
		}

		ferr := ioutil.WriteFile("/dev/termination-log", []byte(err.Error()), 0644)
		if ferr != nil {
			log.WithError(err).Error("cannot write termination log")
		}

		log.WithError(err).Fatal("content initialization failed")
	}()

	fn := "/workspace/.gitpod/content.json"
	f, err := os.Open(fn)
	if os.IsNotExist(err) {
		log.WithError(err).Info("no content init descriptor found - not trying to run it")

		// If there is no content descriptor the content must have come from somewhere (i.e. a layer or ws-daemon).
		// Let's wait for that to happen.
		// TODO: rewrite using fsnotify
		t := time.NewTicker(100 * time.Millisecond)
		for range t.C {
			b, err := ioutil.ReadFile("/workspace/.gitpod/ready")
			if err != nil {
				if !os.IsNotExist(err) {
					log.WithError(err).Error("cannot read content ready file")
				}
				continue
			}

			var m csapi.WorkspaceReadyMessage
			err = json.Unmarshal(b, &m)
			if err != nil {
				log.WithError(err).Fatal("cannot unmarshal content ready file")
				continue
			}

			log.WithField("source", m.Source).Info("supervisor: workspace content available")
			cst.MarkContentReady(m.Source)
			t.Stop()
			break
		}

		err = nil
		return
	}
	if err != nil {
		log.WithError(err).Error("cannot open init descriptor")
		return
	}

	src, err := executor.Execute(ctx, "/workspace", f, initializer.WithInWorkspace)
	if err != nil {
		return
	}

	err = os.Remove(fn)
	if os.IsNotExist(err) {
		// file is gone - we're good
		err = nil
	}
	if err != nil {
		return
	}

	log.WithField("source", src).Info("supervisor: workspace content init finished")
	cst.MarkContentReady(src)
}

func teardown(withDaemonCall bool) {
	if withDaemonCall {
		log.Info("asking ws-daemon to tear down this workspace")
		client, conn, err := ConnectToInWorkspaceDaemonService(context.Background())
		if err != nil {
			log.WithError(err).Error("ungraceful shutdown - teardown was unsuccessful")
			return
		}

		defer conn.Close()
		_, err = client.Teardown(context.Background(), &daemon.TeardownRequest{})
		if err != nil {
			log.WithError(err).Error("ungraceful shutdown - teardown was unsuccessful")
		}
	}
}

// ConnectToInWorkspaceDaemonService attempts to connect to the InWorkspaceService offered by the ws-daemon.
func ConnectToInWorkspaceDaemonService(ctx context.Context) (daemon.InWorkspaceServiceClient, *grpc.ClientConn, error) {
	const socketFN = "/.workspace/daemon.sock"

	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		if _, err := os.Stat(socketFN); err == nil {
			break
		}

		select {
		case <-t.C:
			continue
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("socket did not appear before context was canceled")
		}
	}

	conn, err := grpc.DialContext(ctx, "unix://"+socketFN, grpc.WithInsecure())
	if err != nil {
		return nil, nil, err
	}
	return daemon.NewInWorkspaceServiceClient(conn), conn, nil
}
