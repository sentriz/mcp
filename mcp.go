package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/google/shlex"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	redirectURI       = "http://localhost:8085/oauth/callback"
	daemonIdleTimeout = 30 * time.Minute
)

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  mcp server                                # list configured servers
  mcp server <server> tools                 # list a server's tools
  mcp server <server> schema <tool>         # print a tool's input schema
  mcp server <server> call <tool> [args...] # call a tool
  mcp server <server> login                 # run the oauth login flow
  mcp server <server> stop                  # stop a persistent server daemon
  mcp usage                                 # print this usage

args:
  a single json object or key=value pairs, where each value is
  parsed as json, falling back to a plain string:

  mcp server linear call get_issue id=ABC-123
  mcp server linear call get_issue '{"id": "ABC-123"}'
  mcp server linear call list_issues limit=10 query="foo bar"

config:
  $XDG_CONFIG_HOME/mcp/config.toml
`)
}

func main() {
	var exit int
	defer func() {
		os.Exit(exit)
	}()

	log.SetFlags(0)
	log.SetPrefix("mcp: ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var server, tool string
	var arguments []string

	var err error
	switch args := os.Args[1:]; {
	case match(args, "usage"):
		usage()
	case match(args, "server"):
		err = listServers()
	case match(args, "server", &server, "tools"):
		err = listTools(ctx, server)
	case match(args, "server", &server, "schema", &tool):
		err = showSchema(ctx, server, tool)
	case match(args, "server", &server, "call", &tool, &arguments):
		err = callTool(ctx, server, tool, arguments)
	case match(args, "server", &server, "login"):
		err = login(ctx, server)
	case match(args, "server", &server, "stop"):
		err = stopDaemon(server)
	case match(args, "server", &server, "daemon"):
		err = runDaemon(ctx, server)
	default:
		usage()
		exit = 1
		return
	}
	if err != nil {
		log.Println(err)
		exit = 1
	}
}

func match(args []string, pattern ...any) bool {
	for i, p := range pattern {
		switch p := p.(type) {
		case string:
			if i >= len(args) || args[i] != p {
				return false
			}
		case *string:
			if i >= len(args) {
				return false
			}
			*p = args[i]
		case *[]string:
			*p = args[i:]
			return true
		}
	}
	return len(args) == len(pattern)
}

func listServers() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	for _, srv := range cfg.Servers {
		fmt.Printf("%s\t%s\n", srv.Name, srv.Transport)
	}
	return nil
}

func listTools(ctx context.Context, server string) error {
	tools, err := getTools(ctx, server)
	if err != nil {
		return err
	}
	for _, t := range tools {
		fmt.Printf("%s\t%s\n", t.Name, t.Description)
	}
	srv, err := getServer(server)
	if err != nil {
		return err
	}
	if srv.Instructions != "" {
		fmt.Printf("\n%s\n", strings.TrimSpace(srv.Instructions))
	}
	return nil
}

func showSchema(ctx context.Context, server, tool string) error {
	tools, err := getTools(ctx, server)
	if err != nil {
		return err
	}
	for _, t := range tools {
		if t.Name != tool {
			continue
		}
		out, err := json.MarshalIndent(t.InputSchema, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}
	return fmt.Errorf("tool %q not found on server %q", tool, server)
}

func getTools(ctx context.Context, server string) ([]mcp.Tool, error) {
	resp, err := daemonRoundTrip(ctx, server, rpcRequest{Method: "tools"})
	if err != nil {
		return nil, err
	}
	var tools []mcp.Tool
	if err := json.Unmarshal(resp.Result, &tools); err != nil {
		return nil, err
	}
	return tools, nil
}

func callTool(ctx context.Context, server, tool string, arguments []string) error {
	args, err := parseArgs(arguments)
	if err != nil {
		return err
	}

	resp, err := daemonRoundTrip(ctx, server, rpcRequest{Method: "call", Tool: tool, Args: args})
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, resp.Result, "", "  "); err != nil {
		return err
	}

	if resp.IsError {
		log.Println(buf.String())
		return errors.New("tool returned an error")
	}
	fmt.Println(buf.String())
	return nil
}

func parseArgs(arguments []string) (map[string]any, error) {
	if len(arguments) == 0 {
		return nil, nil
	}

	if len(arguments) == 1 && structured(arguments[0]) {
		var args map[string]any
		if err := json.Unmarshal([]byte(arguments[0]), &args); err != nil {
			return nil, fmt.Errorf("invalid arguments json: %w", err)
		}
		return args, nil
	}

	args := make(map[string]any, len(arguments))
	for _, kv := range arguments {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("expected key=value, got %q", kv)
		}
		var parsed any
		if err := json.Unmarshal([]byte(v), &parsed); err != nil {
			if structured(v) {
				return nil, fmt.Errorf("invalid json for %q: %w", k, err)
			}
			parsed = v
		}
		args[k] = parsed
	}
	return args, nil
}

func structured(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[")
}

type rpcRequest struct {
	Method string         `json:"method"`
	Tool   string         `json:"tool,omitempty"`
	Args   map[string]any `json:"args"`
}

type rpcResponse struct {
	Error   string          `json:"error,omitempty"`
	IsError bool            `json:"isError,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

func daemonRoundTrip(ctx context.Context, server string, req rpcRequest) (rpcResponse, error) {
	conn, err := dialDaemon(ctx, server)
	if err != nil {
		return rpcResponse{}, err
	}
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { conn.Close() })()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return rpcResponse{}, err
	}
	var resp rpcResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return rpcResponse{}, err
	}
	if resp.Error != "" {
		return rpcResponse{}, errors.New(resp.Error)
	}
	return resp, nil
}

func dialDaemon(ctx context.Context, server string) (net.Conn, error) {
	var d net.Dialer
	sock := daemonSocket(server)
	conn, err := d.DialContext(ctx, "unix", sock)
	if err == nil {
		return conn, nil
	}

	if _, err := getServer(server); err != nil {
		return nil, err
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logFile, err := os.Create(daemonLog(server))
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "server", server, "daemon")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = cmd.Start()
	logFile.Close()
	if err != nil {
		return nil, err
	}

	for range 50 {
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		conn, err = d.DialContext(ctx, "unix", sock)
		if err == nil {
			return conn, nil
		}
	}
	return nil, fmt.Errorf("daemon for %q did not start, see %s: %w", server, daemonLog(server), err)
}

func runDaemon(ctx context.Context, server string) error {
	pidFile := daemonPid(server)
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return err
	}
	defer os.Remove(pidFile)

	sock := daemonSocket(server)
	os.Remove(sock)
	l, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer os.Remove(sock)
	defer l.Close()

	d := &daemon{server: server}
	defer func() {
		if d.client != nil {
			d.client.Close()
		}
	}()

	cancelled := context.AfterFunc(ctx, func() { l.Close() })
	defer cancelled()

	idle := time.AfterFunc(daemonIdleTimeout, func() { l.Close() })
	for {
		conn, err := l.Accept()
		if err != nil {
			return nil
		}
		idle.Stop()
		serveConn(ctx, d, conn)
		idle.Reset(daemonIdleTimeout)
	}
}

type daemon struct {
	server string
	client *client.Client
	tools  []mcp.Tool
}

func serveConn(ctx context.Context, d *daemon, conn net.Conn) {
	defer conn.Close()

	var req rpcRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	resp := d.handle(ctx, req)
	if err := json.NewEncoder(conn).Encode(resp); err != nil {
		log.Println("write response:", err)
	}
}

func (d *daemon) handle(ctx context.Context, req rpcRequest) rpcResponse {
	switch req.Method {
	case "tools":
		tools, err := d.listTools(ctx)
		if err != nil {
			return rpcResponse{Error: err.Error()}
		}
		return result(tools, false)
	case "call":
		c, err := d.getClient(ctx)
		if err != nil {
			return rpcResponse{Error: err.Error()}
		}
		var creq mcp.CallToolRequest
		creq.Params.Name = req.Tool
		creq.Params.Arguments = req.Args
		res, err := c.CallTool(ctx, creq)
		if err != nil {
			return rpcResponse{Error: err.Error()}
		}
		return result(res.Content, res.IsError)
	default:
		return rpcResponse{Error: fmt.Sprintf("unknown method %q", req.Method)}
	}
}

func (d *daemon) listTools(ctx context.Context) ([]mcp.Tool, error) {
	if d.tools != nil {
		return d.tools, nil
	}
	c, err := d.getClient(ctx)
	if err != nil {
		return nil, err
	}
	res, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil, err
	}
	d.tools = res.Tools
	return d.tools, nil
}

func (d *daemon) getClient(ctx context.Context) (*client.Client, error) {
	if d.client != nil {
		return d.client, nil
	}
	c, err := connect(ctx, d.server)
	if client.IsOAuthAuthorizationRequiredError(err) {
		return nil, fmt.Errorf("not authorized for %q; run: mcp server %s login", d.server, d.server)
	}
	if err != nil {
		return nil, err
	}
	d.client = c
	return c, nil
}

func result(v any, isError bool) rpcResponse {
	b, err := json.Marshal(v)
	if err != nil {
		return rpcResponse{Error: err.Error()}
	}
	return rpcResponse{IsError: isError, Result: b}
}

func stopDaemon(server string) error {
	b, err := os.ReadFile(daemonPid(server))
	if err != nil {
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return err
	}
	syscall.Kill(pid, syscall.SIGTERM)
	os.Remove(daemonPid(server))
	os.Remove(daemonSocket(server))
	return nil
}

func daemonSocket(server string) string {
	return filepath.Join(runtimeDir(), server+".sock")
}

func daemonPid(server string) string {
	return filepath.Join(runtimeDir(), server+".pid")
}

func daemonLog(server string) string {
	return filepath.Join(runtimeDir(), server+"-daemon.log")
}

func runtimeDir() string {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		panic("XDG_RUNTIME_DIR not set")
	}
	dir = filepath.Join(dir, "mcp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		panic(err)
	}
	return dir
}

func getServer(server string) (Server, error) {
	cfg, err := loadConfig()
	if err != nil {
		return Server{}, err
	}
	for _, srv := range cfg.Servers {
		if srv.Name == server {
			return srv, nil
		}
	}
	return Server{}, fmt.Errorf("unknown server %q", server)
}

func connect(ctx context.Context, server string) (*client.Client, error) {
	srv, err := getServer(server)
	if err != nil {
		return nil, err
	}
	if err := srv.resolve(ctx); err != nil {
		return nil, err
	}

	c, err := newClient(ctx, srv)
	if err != nil {
		return nil, err
	}

	var init mcp.InitializeRequest
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "mcp", Version: "0.1.0"}
	if _, err := c.Initialize(ctx, init); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func newClient(ctx context.Context, srv Server) (*client.Client, error) {
	switch srv.Transport {
	case "stdio":
		if len(srv.Command) == 0 {
			return nil, fmt.Errorf("server %q has no command", srv.Name)
		}
		env := os.Environ()
		for k, v := range srv.Env {
			env = append(env, k+"="+v)
		}
		return client.NewStdioMCPClient(srv.Command[0], env, srv.Command[1:]...)
	case "http":
		var c *client.Client
		var err error
		if srv.Auth == "oauth" {
			oc, cerr := oauthConfig(srv)
			if cerr != nil {
				return nil, cerr
			}
			c, err = client.NewOAuthStreamableHttpClient(srv.URL, oc)
		} else {
			c, err = client.NewStreamableHttpClient(srv.URL, transport.WithHTTPHeaders(srv.Headers))
		}
		if err != nil {
			return nil, err
		}
		if err := c.Start(ctx); err != nil {
			return nil, err
		}
		return c, nil
	default:
		return nil, fmt.Errorf("server %q has unknown transport %q", srv.Name, srv.Transport)
	}
}

func login(ctx context.Context, server string) error {
	srv, err := getServer(server)
	if err != nil {
		return err
	}
	if srv.Auth != "oauth" {
		return fmt.Errorf("server %q is not configured for oauth", server)
	}

	c, err := connect(ctx, server)
	if err == nil {
		c.Close()
		return nil
	}
	if !client.IsOAuthAuthorizationRequiredError(err) {
		return err
	}
	return authorize(ctx, client.GetOAuthHandler(err))
}

func authorize(ctx context.Context, h *transport.OAuthHandler) error {
	verifier, err := client.GenerateCodeVerifier()
	if err != nil {
		return err
	}
	state, err := client.GenerateState()
	if err != nil {
		return err
	}

	if h.GetClientID() == "" {
		if err := h.RegisterClient(ctx, "mcp"); err != nil {
			return fmt.Errorf("register client: %w", err)
		}
	}

	authURL, err := h.GetAuthorizationURL(ctx, state, client.GenerateCodeChallenge(verifier))
	if err != nil {
		return err
	}

	callback, err := awaitCallback(ctx, authURL)
	if err != nil {
		return err
	}
	if callback["state"] != state {
		return errors.New("oauth state mismatch")
	}
	code := callback["code"]
	if code == "" {
		return fmt.Errorf("authorization failed: %s", callback["error"])
	}
	return h.ProcessAuthorizationResponse(ctx, code, state, verifier)
}

func awaitCallback(ctx context.Context, authURL string) (map[string]string, error) {
	redirect, err := url.Parse(redirectURI)
	if err != nil {
		return nil, err
	}

	result := make(chan map[string]string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(redirect.Path, func(w http.ResponseWriter, r *http.Request) {
		params := map[string]string{}
		for k, v := range r.URL.Query() {
			if len(v) > 0 {
				params[k] = v[0]
			}
		}
		io.WriteString(w, "authorization complete, you can close this window")
		result <- params
	})

	srv := &http.Server{Addr: redirect.Host, Handler: mux}
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.ListenAndServe() }()
	defer srv.Close()

	log.Println(authURL)

	select {
	case params := <-result:
		return params, nil
	case err := <-srvErr:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func oauthConfig(srv Server) (client.OAuthConfig, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return client.OAuthConfig{}, err
	}
	path := filepath.Join(dir, "mcp", srv.Name+"-token.json")
	return client.OAuthConfig{
		ClientID:              srv.ClientID,
		ClientSecret:          srv.ClientSecret,
		RedirectURI:           redirectURI,
		Scopes:                srv.Scopes,
		TokenStore:            fileTokenStore{path: path},
		PKCEEnabled:           true,
		AuthServerMetadataURL: srv.AuthMetadataURL,
	}, nil
}

type fileTokenStore struct{ path string }

func (s fileTokenStore) GetToken(ctx context.Context) (*transport.Token, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, transport.ErrNoToken
	}
	if err != nil {
		return nil, err
	}
	var token transport.Token
	if err := json.Unmarshal(b, &token); err != nil {
		return nil, err
	}
	return &token, nil
}

func (s fileTokenStore) SaveToken(ctx context.Context, token *transport.Token) error {
	b, err := json.Marshal(token)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}

type Config struct {
	Servers []Server `toml:"server"`
}

type Server struct {
	Name            string            `toml:"name"`
	Transport       string            `toml:"transport"`
	Command         []string          `toml:"command"`
	Env             map[string]string `toml:"env"`
	URL             string            `toml:"url"`
	Headers         map[string]string `toml:"headers"`
	Auth            string            `toml:"auth"`
	Scopes          []string          `toml:"scopes"`
	ClientID        string            `toml:"client_id"`
	ClientSecret    string            `toml:"client_secret"`
	AuthMetadataURL string            `toml:"auth_metadata_url"`
	Instructions    string            `toml:"instructions"`
}

func loadConfig() (Config, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return Config{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "mcp", "config.toml"))
	if err != nil {
		return Config{}, err
	}

	var cfg Config
	if err := toml.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

func (srv *Server) resolve(ctx context.Context) error {
	var errs error
	repl := func(s string) string {
		return os.Expand(s, func(key string) string {
			command, ok := strings.CutPrefix(key, "exec ")
			if !ok {
				return os.Getenv(key)
			}
			val, err := runCommand(ctx, command)
			errs = errors.Join(errs, err)
			return val
		})
	}

	srv.URL = repl(srv.URL)
	srv.ClientID = repl(srv.ClientID)
	srv.ClientSecret = repl(srv.ClientSecret)
	for i := range srv.Command {
		srv.Command[i] = repl(srv.Command[i])
	}
	for k := range srv.Headers {
		srv.Headers[k] = repl(srv.Headers[k])
	}
	for k := range srv.Env {
		srv.Env[k] = repl(srv.Env[k])
	}
	return errs
}

func runCommand(ctx context.Context, command string) (string, error) {
	fields, err := shlex.Split(command)
	if err != nil {
		return "", fmt.Errorf("split %q: %w", command, err)
	}
	if len(fields) == 0 {
		return "", errors.New("empty command")
	}
	out, err := exec.CommandContext(ctx, fields[0], fields[1:]...).Output()
	if err != nil {
		return "", fmt.Errorf("run %q: %w", command, err)
	}
	return strings.TrimSpace(string(out)), nil
}
