package ui

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// Serve listens on 127.0.0.1 and returns the page URL. It stops when ctx is done.
func Serve(ctx context.Context, c *Controller) (string, error) {
	page, _, err := Listen(ctx, c, "")
	return page, err
}

// Listen serves the page on addr. An empty addr uses a free loopback port.
// token is non-empty when other machines can open addr. The operator sets it
// with PEERSYNC_UI_TOKEN and types it into the page.
func Listen(ctx context.Context, c *Controller, addr string) (page, token string, err error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", "", err
	}
	if exposed(addr) {
		token, err = c.UIToken()
		if err != nil {
			_ = ln.Close()
			return "", "", err
		}
	}
	srv := &http.Server{Handler: handler(c, token)}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	go func() { _ = srv.Serve(ln) }()
	return pageURL(ln), token, nil
}

func exposed(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return true
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(host)
	return ip == nil || !ip.IsLoopback()
}

func pageURL(ln net.Listener) string {
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return "http://" + ln.Addr().String() + "/"
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "0.0.0.0"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

func handler(c *Controller, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		serveAsset(w, "assets/index.html", "text/html; charset=utf-8")
	})
	mux.HandleFunc("GET /app.css", func(w http.ResponseWriter, r *http.Request) {
		serveAsset(w, "assets/app.css", "text/css; charset=utf-8")
	})
	mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) {
		serveAsset(w, "assets/app.js", "text/javascript; charset=utf-8")
	})
	mux.HandleFunc("GET /logo.png", func(w http.ResponseWriter, r *http.Request) {
		serveAsset(w, "assets/logo.png", "image/png")
	})
	mux.HandleFunc("GET /api/state", apiWith(token, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, c.State())
	}))
	mux.HandleFunc("POST /api/settings", apiWith(token, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			DeviceName string `json:"device_name"`
			Server     string `json:"server"`
			ListenPort int    `json:"listen_port"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		if err := c.UpdateSettings(body.DeviceName, body.Server, body.ListenPort); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, c.State())
	}))
	mux.HandleFunc("POST /api/start", apiWith(token, func(w http.ResponseWriter, r *http.Request) {
		c.Start()
		writeJSON(w, http.StatusOK, c.State())
	}))
	mux.HandleFunc("POST /api/stop", apiWith(token, func(w http.ResponseWriter, r *http.Request) {
		c.Stop()
		writeJSON(w, http.StatusOK, c.State())
	}))
	mux.HandleFunc("POST /api/pick", apiWith(token, func(w http.ResponseWriter, r *http.Request) {
		path, err := c.PickFolder()
		if errors.Is(err, ErrCancelled) {
			writeJSON(w, http.StatusOK, map[string]string{"path": ""})
			return
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"path": path})
	}))
	mux.HandleFunc("POST /api/shares", apiWith(token, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Path  string `json:"path"`
			Label string `json:"label"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		code, id, label, abs, err := c.CreateShare(body.Path, body.Label)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"invite":   code,
			"share_id": id,
			"label":    label,
			"path":     abs,
		})
	}))
	mux.HandleFunc("POST /api/shares/join", apiWith(token, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Invite string `json:"invite"`
			Path   string `json:"path"`
			Label  string `json:"label"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		id, abs, server, err := c.JoinShare(body.Invite, body.Path, body.Label)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"share_id": id,
			"path":     abs,
			"server":   server,
		})
	}))
	mux.HandleFunc("POST /api/shares/invite", apiWith(token, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ShareID string `json:"share_id"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		code, err := c.Invite(body.ShareID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"invite": code})
	}))
	mux.HandleFunc("POST /api/shares/remove", apiWith(token, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ShareID string `json:"share_id"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, err)
			return
		}
		if err := c.RemoveShare(body.ShareID); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, c.State())
	}))
	return mux
}

func apiWith(token string, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			if !tokenMatch(r.Header.Get("X-PeerSync-Token"), token) {
				http.Error(w, "forbidden", http.StatusUnauthorized)
				return
			}
			fn(w, r)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Header.Get("X-PeerSync") != "1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		fn(w, r)
	}
}

func tokenMatch(got, want string) bool {
	if len(got) != len(want) || want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func serveAsset(w http.ResponseWriter, name, contentType string) {
	b, err := Assets.ReadFile(name)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

func readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errors.New("请求格式不正确")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	msg := err.Error()
	if errors.Is(err, io.EOF) {
		msg = "请求格式不正确"
	}
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
}
