package server

import (
	"io"
	"net/http"

	"gondar/hosttools"
)

// Version discovery has its own GET path. It never calls POST /jobs and does
// not accept repository, version, digest or URL overrides from a web client.
func (s *Server) hostenvVersion(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := hosttools.Lookup(id); !ok {
		writeErr(w, 400, "未知受管工具")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://hostd/versions/"+id, nil)
	if err != nil {
		writeErr(w, 400, "工具编号非法")
		return
	}
	if r.URL.Query().Get("refresh") == "1" {
		req.URL.RawQuery = "refresh=1"
	}
	req.Header.Set("X-Gondar-Install-Proxy", s.installProxy())
	client := hostdClient()
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, 503, errConnectHostd(err).Error())
		return
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		writeErr(w, 502, "安装助手版本响应无效")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
}
