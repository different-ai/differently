package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/different-ai/differently/internal/dashboard"
	"github.com/different-ai/differently/pkg/api"
	"github.com/different-ai/differently/pkg/client"
)

// The local admin is a web page served by the CLI on this machine. It is the
// management channel, and it is not the orchestrator: it holds no state,
// enforces nothing and stops with the command. What it shows comes from
// `differently discover` (the operator's own tools and sign-ins) and, when
// one is configured and answers, from an orchestrator, reached with the key
// the CLI already uses. The browser never holds that key.

//go:embed localadmin
var localAdminFiles embed.FS

const (
	// localSessionCookie, with the port added, holds the session token;
	// scripts cannot read it.
	localSessionCookie = "dly_local_session"
	// localMarkerCookie tells the dashboard's script that the server in front
	// of it adds the key, so it must not ask for one. Its value is the port.
	localMarkerCookie = "dly_local"
)

// orchestratorStatus is what the local admin knows about the orchestrator
// the CLI points at.
type orchestratorStatus struct {
	URL           string             `json:"url"`
	Reachable     bool               `json:"reachable"`
	Version       string             `json:"version,omitempty"`
	Installation  string             `json:"installation,omitempty"`
	HasKey        bool               `json:"hasKey"`
	Authenticated bool               `json:"authenticated"`
	KeyID         string             `json:"keyId,omitempty"`
	Scopes        []string           `json:"scopes,omitempty"`
	Problem       string             `json:"problem,omitempty"`
	Pools         []orchestratorPool `json:"pools"`
}

type orchestratorPool struct {
	Pool   string `json:"pool"`
	Trust  string `json:"trust"`
	Health string `json:"health"`
}

// localAdmin serves the local admin for one run of `differently console`.
type localAdmin struct {
	// token is this run's secret. The address the command prints carries it
	// once; after that a cookie does.
	token string
	// port is the listener's. Cookies are shared by every port of a host, so
	// both cookies carry it: two runs do not clash, and a dashboard served by
	// a real orchestrator on another port does not take itself for this one.
	port string
	// hosts are the Host headers the listener answers to. Anything else is
	// another site's name resolved to this machine.
	hosts map[string]bool
	orch  settings
	// setupFile is the setup file the page shows and can apply
	// (console_setup.go): an absolute path, which need not exist.
	setupFile string
}

func newLocalAdmin(orch settings, addr net.Addr) (*localAdmin, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	_, port, _ := net.SplitHostPort(addr.String())
	return &localAdmin{
		token: hex.EncodeToString(b),
		port:  port,
		hosts: map[string]bool{"127.0.0.1:" + port: true, "localhost:" + port: true},
		orch:  orch,
	}, nil
}

func (a *localAdmin) handler() http.Handler {
	mux := http.NewServeMux()
	pages, _ := fs.Sub(localAdminFiles, "localadmin")
	mux.Handle("/", getOnly(secureHeaders(http.FileServerFS(pages))))
	mux.Handle("/dashboard/", getOnly(http.StripPrefix("/dashboard/", dashboard.Handler())))
	mux.HandleFunc("GET /local/discover", func(w http.ResponseWriter, r *http.Request) {
		writeLocalJSON(w, discover(r.Context()))
	})
	mux.HandleFunc("GET /local/orchestrator", func(w http.ResponseWriter, r *http.Request) {
		writeLocalJSON(w, a.orchestrator(r.Context()))
	})
	mux.HandleFunc("GET /local/setup", a.getSetup)
	mux.HandleFunc("POST /local/setup/apply", a.applySetup)
	proxy := a.proxy()
	mux.Handle("/v1/", proxy)
	mux.Handle("GET /version", proxy)
	return a.guard(mux)
}

func getOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeLocalJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// guard lets in only this run's browser session. The local admin acts with
// the CLI's key and the operator's cloud sign-ins, so a page on another site,
// another name pointed at this machine, or another user of this machine must
// get nothing.
func (a *localAdmin) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.hosts[r.Host] {
			http.Error(w, "this address is not the local admin's", http.StatusForbidden)
			return
		}
		if t := r.URL.Query().Get("token"); t != "" && r.Method == http.MethodGet && r.URL.Path == "/" {
			if !a.isToken(t) {
				http.Error(w, "that token is not this session's; use the address `differently console` printed", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: a.sessionCookie(), Value: a.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.SetCookie(w, &http.Cookie{Name: localMarkerCookie, Value: a.port, Path: "/", SameSite: http.SameSiteStrictMode})
			// Redirect so the token leaves the address bar and the history.
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		c, err := r.Cookie(a.sessionCookie())
		if err != nil || !a.isToken(c.Value) {
			http.Error(w, "open the address `differently console` printed: it carries this session's token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *localAdmin) sessionCookie() string { return localSessionCookie + "_" + a.port }

func (a *localAdmin) isToken(s string) bool {
	return subtle.ConstantTimeCompare([]byte(s), []byte(a.token)) == 1
}

// proxy forwards API calls to the orchestrator with the CLI's key. Nothing
// the browser sent to identify itself goes on: not its cookies, which hold
// this session's token, and not its Authorization header.
func (a *localAdmin) proxy() http.Handler {
	target, err := url.Parse(a.orch.URL)
	if err != nil || target.Host == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "no orchestrator is configured", http.StatusBadGateway)
		})
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Referer")
			pr.Out.Header.Del("Authorization")
			if a.orch.APIKey != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+a.orch.APIKey)
			}
		},
		// Streams (exec output, events) must not wait for a full buffer.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "unavailable", "message": "the orchestrator at " + a.orch.URL + " does not answer"}})
		},
	}
}

// orchestrator asks the configured orchestrator who it is and what it has.
// One that does not answer is a normal state here, not an error.
func (a *localAdmin) orchestrator(ctx context.Context) orchestratorStatus {
	st := orchestratorStatus{URL: a.orch.URL, HasKey: a.orch.APIKey != "", Pools: []orchestratorPool{}}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	c := client.New(a.orch.URL, a.orch.APIKey)
	c.Retries = 0
	var v api.VersionInfo
	if err := c.Do(ctx, "GET", "/version", nil, &v); err != nil {
		st.Problem = "nothing answers at " + a.orch.URL
		return st
	}
	st.Reachable, st.Version, st.Installation = true, v.Version, v.Installation
	if !st.HasKey {
		st.Problem = "no key is saved for it; run: differently login"
		return st
	}
	var me struct {
		KeyID  string   `json:"keyId"`
		Scopes []string `json:"scopes"`
	}
	if err := c.Do(ctx, "GET", "/v1/me", nil, &me); err != nil {
		st.Problem = "it refused the saved key; run: differently login"
		return st
	}
	st.Authenticated, st.KeyID, st.Scopes = true, me.KeyID, me.Scopes
	var caps struct {
		Items []orchestratorPool `json:"items"`
	}
	if err := c.Do(ctx, "GET", "/v1/capabilities", nil, &caps); err == nil && caps.Items != nil {
		st.Pools = caps.Items
	}
	return st
}

// serveLocalAdmin serves until ctx ends.
func serveLocalAdmin(ctx context.Context, ln net.Listener, a *localAdmin) error {
	srv := &http.Server{Handler: a.handler(), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(stop)
		return nil
	}
}

// openBrowser is replaced in tests.
var openBrowser = func(address string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", address).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", address).Start()
	default:
		return exec.Command("xdg-open", address).Start()
	}
}

func consoleCmd() *cobra.Command {
	var port int
	var noOpen bool
	var file string
	cmd := &cobra.Command{
		Use:   "console",
		Short: "Open the local admin: a page on this machine for what you have and what runs",
		Long: `Starts the local admin on this machine and opens it in a browser. It stops
when the command does. Nothing is deployed and nothing is stored.

It shows what "differently discover" finds through your own cloud tools and,
when an orchestrator is configured and answers, that orchestrator: its pools
and its dashboard, reached with the key this CLI already uses. The browser
never holds the key.

It also shows the setup file (differently.yaml in this directory, or -f):
what differs between it and the orchestrator, as "differently diff" prints
it, with a button that applies it as "differently apply" does. The button
never deletes: pools and templates the file does not name are left alone.

It needs no orchestrator. Without one there is nothing to route sandboxes,
enforce placement policy or quotas, or keep the audit record: the local admin
is for looking and setting up, not for enforcing.

It listens on 127.0.0.1 only. The address it prints carries a token for this
run; other sites, other names for this machine and other users are refused.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				return fmt.Errorf("cannot listen on 127.0.0.1:%d (%w); choose another with --port", port, err)
			}
			a, err := newLocalAdmin(load(), ln.Addr())
			if err != nil {
				ln.Close()
				return err
			}
			if a.setupFile, err = filepath.Abs(file); err != nil {
				ln.Close()
				return err
			}
			address := "http://" + ln.Addr().String() + "/?token=" + a.token
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Local admin: %s\n", address)
			fmt.Fprintf(out, "It runs on this machine only and holds nothing. Orchestrator: %s. Ctrl-C stops it.\n", a.orch.URL)
			if !noOpen {
				if err := openBrowser(address); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "could not open a browser (%v); open the address above\n", err)
				}
			}
			run := cmd.Context()
			if run == nil || run == context.Background() {
				run = ctx()
			}
			if err := serveLocalAdmin(run, ln, a); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&port, "port", 7878, "port on 127.0.0.1 (0 picks a free one)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the address without opening a browser")
	cmd.Flags().StringVarP(&file, "file", "f", defaultApplyFile, "the setup file the page shows and can apply")
	return cmd
}
