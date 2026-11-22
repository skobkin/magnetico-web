package server

import (
	"fmt"
	"io"
	"log/slog"
)

// NewLogger builds the service logger (spec §10.1). level is debug, info,
// warn or error; format is text or json.
func NewLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("unknown log level")
	}
	opts := &slog.HandlerOptions{Level: lv}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("unknown log format")
}
