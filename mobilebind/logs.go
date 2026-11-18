package mobilebind

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"
)

// logHook forwards every logrus entry from the library to the host's OnLog.
type logHook struct{ l *Librespot }

func (h *logHook) Levels() []logrus.Level { return logrus.AllLevels }

func (h *logHook) Fire(e *logrus.Entry) error {
	msg := e.Message
	if len(e.Data) > 0 {
		keys := make([]string, 0, len(e.Data))
		for k := range e.Data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString(msg)
		for _, k := range keys {
			fmt.Fprintf(&b, " %s=%v", k, e.Data[k])
		}
		msg = b.String()
	}
	h.l.dispatchLog(e.Level.String(), msg)
	return nil
}

// routeLogs sends library logs to the host callback instead of stderr. With no
// callback, logs keep going to stderr. level is the minimum level forwarded;
// logrus.PanicLevel (the zero value, i.e. "not configured") means info.
func (l *Librespot) routeLogs(level logrus.Level) {
	logger := logrus.StandardLogger()
	if level == logrus.PanicLevel {
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)
	logger.ReplaceHooks(logrus.LevelHooks{})
	if l.eventCallback() == nil {
		logger.SetOutput(os.Stderr)
		return
	}
	logger.AddHook(&logHook{l: l})
	logger.SetOutput(io.Discard)
}
