package workbuddy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type DiscoveredModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

var supportedModelsPattern = regexp.MustCompile(`Currently supported:\s*\(([^)]*)\)`)

func DiscoverModels(ctx context.Context, config Config) ([]DiscoveredModel, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(probeCtx, config.Executable, "--help")
	command.Env = minimalEnvironment(os.TempDir())
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, errors.New("WorkBuddy model discovery failed")
	}
	match := supportedModelsPattern.FindSubmatch(output)
	if len(match) != 2 {
		return nil, errors.New("WorkBuddy did not publish a supported model list")
	}
	parts := strings.Split(string(match[1]), ",")
	models := make([]DiscoveredModel, 0, len(parts))
	for _, part := range parts {
		id := strings.TrimSpace(part)
		if id != "" {
			models = append(models, DiscoveredModel{ID: id, DisplayName: id})
		}
	}
	return models, nil
}
