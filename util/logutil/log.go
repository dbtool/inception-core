// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package logutil

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"

	"github.com/pingcap/errors"
	log "github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	defaultLogTimeFormat = "2006/01/02 15:04:05.000"
	// DefaultLogMaxSize is the default size of log files.
	DefaultLogMaxSize = 300 // MB
	defaultLogFormat  = "text"
	defaultLogLevel   = log.InfoLevel
	// DefaultQueryLogMaxLen is the default max length of the query in the log.
	DefaultQueryLogMaxLen = 2048
)

// FileLogConfig serializes file log related config in toml/json.
type FileLogConfig struct {
	// Log filename, leave empty to disable file log.
	Filename string `toml:"filename" json:"filename"`
	// Is log rotate enabled. TODO.
	LogRotate bool `toml:"log-rotate" json:"log-rotate"`
	// Max size for a single file, in MB.
	MaxSize uint `toml:"max-size" json:"max-size"`
	// Max log keep days, default is never deleting.
	MaxDays uint `toml:"max-days" json:"max-days"`
	// Maximum number of old log files to retain.
	MaxBackups uint `toml:"max-backups" json:"max-backups"`
}

// LogConfig serializes log related config in toml/json.
type LogConfig struct {
	// Log level.
	Level string `toml:"level" json:"level"`
	// Log format. one of json, text, or console.
	Format string `toml:"format" json:"format"`
	// Disable automatic timestamps in output.
	DisableTimestamp bool `toml:"disable-timestamp" json:"disable-timestamp"`
	// File log config.
	File FileLogConfig `toml:"file" json:"file"`
	// SlowQueryFile filename, default to File log config on empty.
	SlowQueryFile string
}

// isSKippedPackageName tests wether path name is on log library calling stack.
func isSkippedPackageName(name string) bool {
	return strings.Contains(name, "github.com/sirupsen/logrus") ||
		strings.Contains(name, "github.com/coreos/pkg/capnslog")
}

// modifyHook injects file name and line pos into log entry.
type contextHook struct{}

type ContextHook = contextHook

// Fire implements logrus.Hook interface
// https://github.com/sirupsen/logrus/issues/63
func (hook *contextHook) Fire(entry *log.Entry) error {
	// for skip := 0; ; skip++ {
	// 	pc, _, _, ok := runtime.Caller(skip)
	// 	if !ok {
	// 		break
	// 	}
	// 	p := runtime.FuncForPC(pc)
	// 	file, line := p.FileLine(0)

	// 	fmt.Printf("skip = %v, pc = %v\n", skip, pc)
	// 	fmt.Printf("  file = %v, line = %d\n", file, line)
	// 	fmt.Printf("  name = %v\n", p.Name())
	// }

	pc := make([]uintptr, 4)
	cnt := runtime.Callers(8, pc)

	for i := 0; i < cnt; i++ {
		fu := runtime.FuncForPC(pc[i] - 1)
		name := fu.Name()
		if !isSkippedPackageName(name) {
			file, line := fu.FileLine(pc[i] - 1)
			entry.Data["file"] = path.Base(file)
			entry.Data["line"] = line
			// entry.Data["file"] = fmt.Sprintf("%s:%d", path.Base(file), line)
			s := strings.Split(name, ".")
			entry.Data["func"] = s[len(s)-1]
			break
		}
	}
	return nil
}

// Levels implements logrus.Hook interface.
func (hook *contextHook) Levels() []log.Level {
	return log.AllLevels
}

func stringToLogLevel(level string) log.Level {
	switch strings.ToLower(level) {
	case "fatal":
		return log.FatalLevel
	case "error":
		return log.ErrorLevel
	case "warn", "warning":
		return log.WarnLevel
	case "debug":
		return log.DebugLevel
	case "info":
		return log.InfoLevel
	}
	return defaultLogLevel
}

// logTypeToColor converts the Level to a color string.
func logTypeToColor(level log.Level) string {
	switch level {
	case log.DebugLevel:
		return "[0;37"
	case log.InfoLevel:
		return "[0;36"
	case log.WarnLevel:
		return "[0;33"
	case log.ErrorLevel:
		return "[0;31"
	case log.FatalLevel:
		return "[0;31"
	case log.PanicLevel:
		return "[0;31"
	}

	return "[0;37"
}

// textFormatter is for compatibility with ngaut/log
type textFormatter struct {
	DisableTimestamp bool
	EnableColors     bool
	EnableEntryOrder bool
}

// Format implements logrus.Formatter
func (f *textFormatter) Format(entry *log.Entry) ([]byte, error) {
	var b *bytes.Buffer
	if entry.Buffer != nil {
		b = entry.Buffer
	} else {
		b = &bytes.Buffer{}
	}

	if f.EnableColors {
		colorStr := logTypeToColor(entry.Level)
		fmt.Fprintf(b, "\033%sm ", colorStr)
	}

	if !f.DisableTimestamp {
		fmt.Fprintf(b, "%s ", entry.Time.Format(defaultLogTimeFormat))
	}
	if file, ok := entry.Data["file"]; ok {
		fmt.Fprintf(b, "%s:%v:", file, entry.Data["line"])
	}
	fmt.Fprintf(b, " [%s] %s", entry.Level.String(), entry.Message)

	if f.EnableEntryOrder {
		keys := make([]string, 0, len(entry.Data))
		for k := range entry.Data {
			if k != "file" && k != "line" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(b, " %v=%v", k, entry.Data[k])
		}
	} else {
		for k, v := range entry.Data {
			if k != "file" && k != "line" {
				fmt.Fprintf(b, " %v=%v", k, v)
			}
		}
	}

	b.WriteByte('\n')

	if f.EnableColors {
		b.WriteString("\033[0m")
	}
	return b.Bytes(), nil
}

func stringToLogFormatter(format string, disableTimestamp bool) log.Formatter {

	// callerPrettyfier := func(f *runtime.Frame) (string, string) {
	// 	filename := path.Base(f.File)
	// 	s := strings.Split(f.Function, ".")
	// 	funcName := s[len(s)-1]
	// 	return funcName, fmt.Sprintf("%s:%d", filename, f.Line)
	// }

	switch strings.ToLower(format) {
	case "text":
		return &log.TextFormatter{
			TimestampFormat:  defaultLogTimeFormat,
			DisableTimestamp: disableTimestamp,
			DisableColors:    true,
			// CallerPrettyfier: callerPrettyfier,
		}
	case "json":
		return &log.JSONFormatter{
			TimestampFormat:  defaultLogTimeFormat,
			DisableTimestamp: disableTimestamp,
			// CallerPrettyfier: callerPrettyfier,
		}
	case "console":
		return &log.TextFormatter{
			FullTimestamp:    true,
			TimestampFormat:  defaultLogTimeFormat,
			DisableTimestamp: disableTimestamp,
			// CallerPrettyfier: callerPrettyfier,
			DisableColors: false,
		}
	case "highlight":
		return &log.TextFormatter{
			TimestampFormat:  defaultLogTimeFormat,
			DisableTimestamp: disableTimestamp,
			DisableColors:    false,
			// CallerPrettyfier: callerPrettyfier,
		}
	default:
		return &log.TextFormatter{
			TimestampFormat: defaultLogTimeFormat,
			// CallerPrettyfier: callerPrettyfier,
		}
	}
}

// initFileLog initializes file based logging options.
func initFileLog(cfg *FileLogConfig, logger *log.Logger) error {
	if st, err := os.Stat(cfg.Filename); err == nil {
		if st.IsDir() {
			return errors.New("can't use directory as log file name")
		}
	}
	if cfg.MaxSize == 0 {
		cfg.MaxSize = DefaultLogMaxSize
	}

	// use lumberjack to logrotate
	output := &lumberjack.Logger{
		Filename:   cfg.Filename,
		MaxSize:    int(cfg.MaxSize),
		MaxBackups: int(cfg.MaxBackups),
		MaxAge:     int(cfg.MaxDays),
		LocalTime:  true,
	}

	if logger == nil {
		log.SetOutput(output)
	} else {
		logger.Out = output
	}
	return nil
}

// InitLogger initializes PD's logger.
func InitLogger(cfg *LogConfig) error {
	log.SetLevel(stringToLogLevel(cfg.Level))
	log.AddHook(&contextHook{})

	if cfg.Format == "" {
		cfg.Format = defaultLogFormat
	}
	formatter := stringToLogFormatter(cfg.Format, cfg.DisableTimestamp)
	log.SetFormatter(formatter)
	// log.SetReportCaller(true)

	if len(cfg.File.Filename) != 0 {
		if err := initFileLog(&cfg.File, nil); err != nil {
			return errors.Trace(err)
		}
	}

	return nil
}
