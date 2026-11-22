package vdisk

import (
	"embed"
	"encoding/json"
	"net/http"
)

//go:embed web/index.html
var webFS embed.FS

// ChainNode 是镜像链上的一层。
type ChainNode struct {
	Path     string `json:"path"`
	BasePath string `json:"basePath"`
	ReadOnly bool   `json:"readOnly"`
	Dirty    bool   `json:"dirty"`
}

// Server 提供镜像结构检视页面与 JSON 接口。
type Server struct {
	top    *Image
	topPath string
	// chain 从最底层底包到最顶层，顺序由调用方给好。
	chain []ChainNode
	mux   *http.ServeMux
}

// NewServer 用顶层镜像构造 HTTP 服务。chain 给出每层的路径展示信息。
func NewServer(top *Image, topPath string, chain []ChainNode) *Server {
	s := &Server{top: top, topPath: topPath, chain: chain, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /", s.handleIndex)
	s.mux.HandleFunc("GET /api/inspect", s.handleInspect)
	s.mux.HandleFunc("GET /api/check", s.handleCheck)
	s.mux.HandleFunc("POST /api/repair", s.handleRepair)
	s.mux.HandleFunc("GET /api/stats", s.handleStats)
	s.mux.HandleFunc("GET /api/chain", s.handleChain)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (s *Server) handleInspect(w http.ResponseWriter, r *http.Request) {
	rep, err := s.top.Inspect(s.topPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rep)
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	rep, err := s.top.Check(s.topPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, rep)
}

func (s *Server) handleRepair(w http.ResponseWriter, r *http.Request) {
	if s.top.ReadOnly() {
		http.Error(w, ErrReadOnly.Error(), http.StatusConflict)
		return
	}
	rep, err := s.top.Repair(s.topPath)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error(), "report": rep})
		return
	}
	writeJSON(w, rep)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st := s.top.Stats()
	writeJSON(w, map[string]any{
		"logicalReadBytes":  st.LogicalReadBytes,
		"logicalWriteBytes": st.LogicalWriteBytes,
		"physicalReadBytes": st.PhysicalReadBytes,
		"physicalWriteBytes": st.PhysicalWriteBytes,
		"syncCount":         st.SyncCount,
		"readAmp":           st.ReadAmp(),
		"writeAmp":          st.WriteAmp(),
	})
}

func (s *Server) handleChain(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"chain": s.chain})
}

// ListenAndServe 启动 HTTP 服务。
func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.mux)
}
