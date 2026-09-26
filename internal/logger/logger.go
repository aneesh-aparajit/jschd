package logger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	FormatConsole string = "console"
	FormatJSON    string = "json"
)

type Config struct {
	Level  string `json:"level" yaml:"level"`
	Format string `json:"format" yaml:"format"`
}

func New(cfg Config) (*zap.Logger, error) {
	level := zapcore.InfoLevel
	if cfg.Level != "" {
		if err := level.UnmarshalText([]byte(strings.ToLower(cfg.Level))); err != nil {
			return nil, fmt.Errorf("invalid log level %q: %w", cfg.Level, err)
		}
	}

	var enc zapcore.Encoder
	switch cfg.Format {
	case FormatConsole, "":
		ec := zap.NewDevelopmentEncoderConfig()
		ec.EncodeLevel = zapcore.CapitalColorLevelEncoder
		ec.EncodeTime = zapcore.TimeEncoderOfLayout("15:04:05.000")
		enc = zapcore.NewConsoleEncoder(ec)
	case FormatJSON:
		ec := zap.NewProductionEncoderConfig()
		ec.TimeKey = "ts"
		ec.EncodeTime = zapcore.ISO8601TimeEncoder
		ec.EncodeDuration = zapcore.MillisDurationEncoder
		enc = zapcore.NewJSONEncoder(ec)
	default:
		return nil, fmt.Errorf("invalid log format %q (want console or json)", cfg.Format)
	}

	core := zapcore.NewCore(enc, zapcore.Lock(os.Stdout), level)
	return zap.New(core,
		zap.AddCaller(),
		zap.AddStacktrace(zapcore.ErrorLevel),
		zap.ErrorOutput(zapcore.Lock(os.Stderr)),
	), nil
}

func Sync(l *zap.Logger) error {
	err := l.Sync()
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTTY) {
		return nil
	}
	return err
}

type ctxKey struct{}

func WithContext(ctx context.Context, l *zap.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

func FromContext(ctx context.Context) *zap.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*zap.Logger); ok {
		return l
	}
	return zap.L()
}
