// Package whatsapp owns the gateway's WhatsApp connection: the session store, pairing and staying connected.
package whatsapp

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"

	waBinary "go.mau.fi/whatsmeow/binary"
	waLog "go.mau.fi/whatsmeow/util/log"
)

var jid = regexp.MustCompile(`[0-9]+(?:[.:-][0-9]+)*@(?:s\.whatsapp\.net|c\.us|lid|g\.us|broadcast|newsletter|bot|hosted(?:\.lid)?)\b`)

// whatsmeow logs at debug level can include message contents, so they are never passed on. Other
// lines name people by JID and sometimes dump raw protocol messages; both are masked.
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
	for i, arg := range args {
		switch node := arg.(type) {
		case *waBinary.Node:
			args[i] = "<" + node.Tag + " node>"
		case waBinary.Node:
			args[i] = "<" + node.Tag + " node>"
		}
	}
	l.logger.Log(context.Background(), level, jid.ReplaceAllString(fmt.Sprintf(msg, args...), "<jid>"))
}

func (l logAdapter) Errorf(msg string, args ...any) { l.log(slog.LevelError, msg, args) }
func (l logAdapter) Warnf(msg string, args ...any)  { l.log(slog.LevelWarn, msg, args) }
func (l logAdapter) Infof(msg string, args ...any)  { l.log(slog.LevelInfo, msg, args) }
func (l logAdapter) Debugf(string, ...any)          {}

func (l logAdapter) Sub(module string) waLog.Logger {
	return logAdapter{logger: l.logger.With("submodule", module)}
}
