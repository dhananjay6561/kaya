// Package whatsapp owns the gateway's WhatsApp connection: the session store, pairing and staying connected.
package whatsapp

import (
	"context"
	"fmt"
	"log/slog"

	waLog "go.mau.fi/whatsmeow/util/log"
)

// whatsmeow logs at debug level can include message contents, so they are never passed on.
type logAdapter struct {
	logger *slog.Logger
}

// NewLogger adapts logger for whatsmeow. Debug lines are dropped; every other line carries module=whatsmeow.
func NewLogger(logger *slog.Logger) waLog.Logger {
	return logAdapter{logger: logger.With("module", "whatsmeow")}
}

func (l logAdapter) log(level slog.Level, msg string, args []any) {
	if !l.logger.Enabled(context.Background(), level) {
		return
	}
	l.logger.Log(context.Background(), level, fmt.Sprintf(msg, args...))
}

func (l logAdapter) Errorf(msg string, args ...any) { l.log(slog.LevelError, msg, args) }
func (l logAdapter) Warnf(msg string, args ...any)  { l.log(slog.LevelWarn, msg, args) }
func (l logAdapter) Infof(msg string, args ...any)  { l.log(slog.LevelInfo, msg, args) }
func (l logAdapter) Debugf(string, ...any)          {}

func (l logAdapter) Sub(module string) waLog.Logger {
	return logAdapter{logger: l.logger.With("submodule", module)}
}
