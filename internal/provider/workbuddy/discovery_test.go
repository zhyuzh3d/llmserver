package workbuddy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverModelsEnrichesCachedCreditMultiplier(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cacheDir := filepath.Join(home, ".codebuddy", "local_storage")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := `[{"data":{"models":[{"id":"hy4-preview","name":"Hy4 Preview","credits":"x0.29 credits"},{"id":"auto","name":"Auto"}]}}]`
	if err := os.WriteFile(filepath.Join(cacheDir, "entry.info"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := fakeWorkBuddy(t, `
echo 'Usage: codebuddy --model <model> Currently supported: (auto, hy4-preview)'
`)
	models, err := DiscoverModels(context.Background(), Config{Executable: executable})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].DisplayName != "Auto" || models[1].DisplayName != "Hy4 Preview" || models[1].CreditMultiplier != "0.29" {
		t.Fatalf("models = %#v", models)
	}
}
