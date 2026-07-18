package tanuki

import (
	"log/slog"
	"os"
)

var logger *slog.Logger

func initLogger() {
	level := slog.LevelInfo

	if os.Getenv("TANUKI_DEBUG") != "" {
		level = slog.LevelDebug
	}

	logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})).With("component", "tanuki")
}
