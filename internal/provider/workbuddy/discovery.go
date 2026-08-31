package workbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type DiscoveredModel struct {
	ID               string `json:"id"`
	DisplayName      string `json:"display_name"`
	CreditMultiplier string `json:"credit_multiplier,omitempty"`
}

var supportedModelsPattern = regexp.MustCompile(`Currently supported:\s*\(([^)]*)\)`)
var creditMultiplierPattern = regexp.MustCompile(`(?i)^\s*x\s*([0-9]+(?:\.[0-9]+)?)`)

func DiscoverModels(ctx context.Context, config Config) ([]DiscoveredModel, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
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
	catalog := readCachedProductCatalog()
	parts := strings.Split(string(match[1]), ",")
	models := make([]DiscoveredModel, 0, len(parts))
	for _, part := range parts {
		id := strings.TrimSpace(part)
		if id != "" {
			model := DiscoveredModel{ID: id, DisplayName: id}
			if cached, exists := catalog[id]; exists {
				if cached.Name != "" {
					model.DisplayName = cached.Name
				}
				if match := creditMultiplierPattern.FindStringSubmatch(cached.Credits); len(match) == 2 {
					model.CreditMultiplier = match[1]
				}
			}
			models = append(models, model)
		}
	}
	return models, nil
}

type cachedProductModel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Credits string `json:"credits"`
}

func readCachedProductCatalog() map[string]cachedProductModel {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(home, ".codebuddy", "local_storage", "*.info"))
	if err != nil {
		return nil
	}
	type candidate struct {
		path    string
		modTime time.Time
	}
	candidates := make([]candidate, 0, len(paths))
	for _, path := range paths {
		info, statErr := os.Stat(path)
		if statErr == nil && info.Mode().IsRegular() && info.Size() <= 4<<20 {
			candidates = append(candidates, candidate{path: path, modTime: info.ModTime()})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].modTime.After(candidates[j].modTime) })
	for _, item := range candidates {
		raw, readErr := os.ReadFile(item.path)
		if readErr != nil {
			continue
		}
		var entries []struct {
			Data struct {
				Models []cachedProductModel `json:"models"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &entries) != nil {
			continue
		}
		for _, entry := range entries {
			if len(entry.Data.Models) == 0 {
				continue
			}
			catalog := make(map[string]cachedProductModel, len(entry.Data.Models))
			for _, model := range entry.Data.Models {
				if model.ID != "" {
					catalog[model.ID] = model
				}
			}
			return catalog
		}
	}
	return nil
}
