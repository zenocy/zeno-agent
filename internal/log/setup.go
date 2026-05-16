// Package log holds the logrus setup helper plus the observation-log GORM
// store. The "log" package name is reused for both because the observation
// log is the conceptual spine of the service; the application's log/logging
// helper is `Setup` here, and tests import logrus directly.
package log

import (
	"time"

	"github.com/sirupsen/logrus"
)

// LoggingConfig holds logrus configuration.
type LoggingConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"` // "json" or "text"
}

// SetupLogging configures the global logrus logger.
func SetupLogging(cfg LoggingConfig) *logrus.Logger {
	level, err := logrus.ParseLevel(cfg.Level)
	if err != nil {
		level = logrus.InfoLevel
	}
	logger := logrus.StandardLogger()
	logger.SetLevel(level)

	switch cfg.Format {
	case "json":
		logger.SetFormatter(&logrus.JSONFormatter{
			TimestampFormat: time.RFC3339Nano,
		})
	default:
		logger.SetFormatter(&logrus.TextFormatter{
			FullTimestamp:   true,
			TimestampFormat: "15:04:05",
			ForceColors:     true,
			PadLevelText:    true,
		})
	}
	return logger
}
