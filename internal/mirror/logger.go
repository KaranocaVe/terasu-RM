package mirror

import (
	"encoding/json"
	"log"
	"os"
	"time"
)

type structuredLogger struct {
	logger *log.Logger
}

func newStructuredLogger() *structuredLogger {
	return &structuredLogger{logger: log.New(os.Stdout, "", 0)}
}

func (l *structuredLogger) Info(msg string, fields map[string]any) {
	l.log("info", msg, fields)
}

func (l *structuredLogger) Error(msg string, fields map[string]any) {
	l.log("error", msg, fields)
}

func (l *structuredLogger) log(level, msg string, fields map[string]any) {
	entry := map[string]any{
		"ts":    time.Now().Format(time.RFC3339Nano),
		"level": level,
		"msg":   msg,
	}
	for k, v := range fields {
		entry[k] = v
	}
	data, err := json.Marshal(entry)
	if err != nil {
		l.logger.Printf("{\"ts\":%q,\"level\":%q,\"msg\":%q,\"error\":%q}", time.Now().Format(time.RFC3339Nano), level, msg, err.Error())
		return
	}
	l.logger.Print(string(data))
}
