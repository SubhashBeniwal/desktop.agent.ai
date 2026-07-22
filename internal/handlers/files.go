package handlers

import (
	"fmt"
	"os"

	"github.com/aioagent/daemon/internal/dispatch"
)

type fileReadReq struct {
	Path string `json:"path"`
}

func (h *Handlers) fileRead(c *dispatch.Ctx) (any, error) {
	var req fileReadReq
	if err := dispatch.Decode(c.Command.Payload, &req); err != nil {
		return nil, err
	}
	data, err := h.FS.Read(req.Path)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"path":    req.Path,
		"size":    len(data),
		"content": string(data),
	}, nil
}

type fileWriteReq struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Mode    uint32 `json:"mode,omitempty"` // octal file mode, e.g. 0644
	Append  bool   `json:"append,omitempty"`
}

func (h *Handlers) fileWrite(c *dispatch.Ctx) (any, error) {
	var req fileWriteReq
	if err := dispatch.Decode(c.Command.Payload, &req); err != nil {
		return nil, err
	}
	if req.Append {
		abs, err := h.FS.Resolve(req.Path)
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(abs, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if _, err := f.WriteString(req.Content); err != nil {
			return nil, err
		}
	} else {
		if err := h.FS.Write(req.Path, []byte(req.Content), os.FileMode(req.Mode)); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"path":    req.Path,
		"written": len(req.Content),
	}, nil
}

type fileListReq struct {
	Path string `json:"path"`
}

func (h *Handlers) fileList(c *dispatch.Ctx) (any, error) {
	var req fileListReq
	if err := dispatch.Decode(c.Command.Payload, &req); err != nil {
		return nil, err
	}
	if req.Path == "" {
		return nil, fmt.Errorf("path is required")
	}
	entries, err := h.FS.List(req.Path)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"path":    req.Path,
		"entries": entries,
	}, nil
}
