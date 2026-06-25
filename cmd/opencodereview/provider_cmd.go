package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	tea "charm.land/bubbletea/v2"

	"github.com/open-code-review/open-code-review/internal/llm"
)

func runConfigProvider() error {
	configPath, err := defaultConfigPath()
	if err != nil {
		return err
	}

	cfg, err := loadOrCreateConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	m := newProviderTUI(cfg, configPath)
	p := tea.NewProgram(m)
	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	final := finalModel.(providerTUIModel)

	if !final.confirmed {
		// TUI persists changes (create/edit/model/add/delete) directly to disk
		// during the session, so the on-disk file is already up to date for any
		// savedInSession operation. No additional post-TUI apply step is needed.
		if final.savedInSession {
			return nil
		}
		fmt.Println("Cancelled.")
		return nil
	}

	result := final.result()

	if result.isManual {
		return applyManualConfig(configPath, cfg, result)
	}

	if result.isCustom {
		return applyCustomProviderConfig(configPath, cfg, result)
	}

	return applyOfficialProviderConfig(configPath, cfg, result)
}

func applyProviderDeletions(configPath string, cfg *Config, names []string) (bool, error) {
	clearedActive := false
	for _, name := range names {
		wasActive, err := deleteCustomProvider(cfg, name)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[ocr] skip delete %q: %v\n", name, err)
			continue
		}
		if wasActive {
			clearedActive = true
		}
		fmt.Printf("Deleted custom provider %q.\n", name)
	}
	if err := saveConfig(configPath, cfg); err != nil {
		return false, err
	}
	return clearedActive, nil
}

func applyModelDeletions(configPath string, cfg *Config, deletedModels map[string][]string) error {
	if len(deletedModels) == 0 {
		return nil
	}
	if cfg.CustomProviders != nil {
		for name, models := range deletedModels {
			entry, ok := cfg.CustomProviders[name]
			if !ok {
				continue
			}
			original := entry.Models
			entry.Models = removeModels(entry.Models, models)
			modelsChanged := len(entry.Models) != len(original)

			entryChanged := modelsChanged
			if modelListContains(models, entry.Model) {
				entry.Model = ""
				entryChanged = true
			}
			if cfg.Provider == name && modelListContains(models, cfg.Model) {
				cfg.Model = ""
			}
			if entryChanged {
				cfg.CustomProviders[name] = entry
				for _, m := range original {
					if !modelListContains(entry.Models, m) {
						fmt.Printf("Deleted model %q from custom provider %q.\n", m, name)
					}
				}
			}
		}
	}
	// Always persist when deletions were requested; in-session TUI edits may
	// have already applied changes to cfg, making a diff-based changed flag unreliable.
	return saveConfig(configPath, cfg)
}

func removeModels(existing, toRemove []string) []string {
	removeSet := make(map[string]struct{}, len(toRemove))
	for _, m := range toRemove {
		removeSet[m] = struct{}{}
	}
	result := make([]string, 0, len(existing))
	for _, m := range existing {
		if _, found := removeSet[m]; found {
			continue
		}
		result = append(result, m)
	}
	return result
}

func applyManualConfig(configPath string, cfg *Config, result providerTUIResult) error {
	if result.url == "" {
		return fmt.Errorf("URL is required for manual configuration")
	}
	if result.model == "" {
		return fmt.Errorf("model is required for manual configuration")
	}

	cfg.Provider = ""
	cfg.Model = ""
	cfg.Llm.URL = result.url
	cfg.Llm.Model = result.model
	cfg.Llm.AuthToken = result.apiKey
	if result.protocol == "anthropic" {
		useAnthropic := true
		cfg.Llm.UseAnthropic = &useAnthropic
	} else {
		useAnthropic := false
		cfg.Llm.UseAnthropic = &useAnthropic
	}

	if err := saveConfig(configPath, cfg); err != nil {
		return err
	}

	fmt.Println("\nManual configuration saved.")
	fmt.Printf("URL: %s\n", result.url)
	fmt.Printf("Protocol: %s\n", result.protocol)
	fmt.Printf("Model: %s\n", result.model)

	fmt.Println("\nTesting connection...")
	if err := runLLMTest(); err != nil {
		fmt.Fprintf(os.Stderr, "Connection test failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "Configuration has been saved. Fix the issue and run 'ocr llm test' to re-verify.")
		return nil
	}

	return nil
}

func applyCustomProviderConfig(configPath string, cfg *Config, result providerTUIResult) error {
	if result.provider == "" {
		return fmt.Errorf("provider name is required")
	}
	if result.model == "" {
		return fmt.Errorf("model is required")
	}

	if cfg.CustomProviders == nil {
		cfg.CustomProviders = make(map[string]ProviderEntry)
	}

	entry := cfg.CustomProviders[result.provider]
	entry.Model = result.model
	if len(result.models) > 0 {
		entry.Models = mergeModelLists([]string{result.model}, result.models)
	}
	if result.url != "" {
		entry.URL = result.url
	}
	if result.protocol != "" {
		entry.Protocol = result.protocol
	}
	if result.authHeader != "" {
		entry.AuthHeader = result.authHeader
	}
	if result.apiKey != "" {
		entry.APIKey = result.apiKey
	}
	cfg.CustomProviders[result.provider] = entry

	if !result.isEdit {
		cfg.Provider = result.provider
		cfg.Model = result.model
	} else if cfg.Provider == result.provider {
		cfg.Model = result.model
	}

	if err := saveConfig(configPath, cfg); err != nil {
		return err
	}

	if result.isEdit {
		if cfg.Provider == result.provider {
			fmt.Printf("\nActive provider %q updated.\n", result.provider)
		} else {
			fmt.Printf("\nCustom provider %q updated (not currently active).\n", result.provider)
		}
		fmt.Printf("Model: %s\n", result.model)
		fmt.Println("\nTip: run 'ocr config model' to switch model later.")
		return nil
	}

	fmt.Printf("\nProvider set to: %s (custom)\n", result.provider)
	fmt.Printf("Model: %s\n", result.model)

	fmt.Println("\nTesting connection...")
	if err := runLLMTest(); err != nil {
		fmt.Fprintf(os.Stderr, "Connection test failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "Provider configuration has been saved. Fix the issue and run 'ocr llm test' to re-verify.")
		return nil
	}

	fmt.Println("\nTip: run 'ocr config model' to switch model later.")
	return nil
}

func applyOfficialProviderConfig(configPath string, cfg *Config, result providerTUIResult) error {
	if result.provider == "" || result.model == "" {
		return fmt.Errorf("provider and model are required")
	}

	preset, isPreset := llm.LookupProvider(result.provider)

	if result.apiKey == "" {
		if isPreset && preset.EnvVar != "" {
			if os.Getenv(preset.EnvVar) == "" {
				return fmt.Errorf("API key is required for provider %s (configure it or set $%s)", result.provider, preset.EnvVar)
			}
		} else {
			return fmt.Errorf("API key is required for provider %s", result.provider)
		}
	}

	if cfg.Providers == nil {
		cfg.Providers = make(map[string]ProviderEntry)
	}

	entry := cfg.Providers[result.provider]
	entry.Model = result.model
	if len(result.models) > 0 {
		entry.Models = mergeModelLists(entry.Models, result.models)
	}
	if result.apiKey != "" {
		entry.APIKey = result.apiKey
	}
	cfg.Providers[result.provider] = entry

	if cfg.Provider != result.provider {
		cfg.Model = ""
	}
	cfg.Provider = result.provider
	cfg.Model = result.model

	if err := saveConfig(configPath, cfg); err != nil {
		return err
	}

	fmt.Printf("\nProvider set to: %s\n", result.provider)
	fmt.Printf("Model: %s\n", result.model)

	fmt.Println("\nTesting connection...")
	if err := runLLMTest(); err != nil {
		fmt.Fprintf(os.Stderr, "Connection test failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "Provider configuration has been saved. Fix the issue and run 'ocr llm test' to re-verify.")
		return nil
	}

	fmt.Println("\nTip: run 'ocr config model' to switch model later.")
	return nil
}

func runConfigModel() error {
	configPath, err := defaultConfigPath()
	if err != nil {
		return err
	}

	cfg, err := loadOrCreateConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if cfg.Provider == "" {
		return fmt.Errorf("no provider configured. Run 'ocr config provider' first")
	}

	currentModel := ""
	provider := llm.Provider{Name: cfg.Provider, DisplayName: cfg.Provider}
	isCustom := false
	if preset, isPreset := llm.LookupProvider(cfg.Provider); isPreset {
		provider = preset
		if entry, ok := cfg.Providers[cfg.Provider]; ok {
			currentModel = activeModelForProvider(cfg, cfg.Provider, entry)
			provider.Models = mergeModelLists(provider.Models, entry.Models)
		}
	} else {
		isCustom = true
		entry, ok := cfg.CustomProviders[cfg.Provider]
		if !ok {
			return fmt.Errorf("provider %q is not configured in custom_providers", cfg.Provider)
		}
		currentModel = activeModelForProvider(cfg, cfg.Provider, entry)
		provider.DisplayName = cfg.Provider + " (custom)"
		provider.Protocol = entry.Protocol
		provider.BaseURL = entry.URL
		provider.Models = mergeModelLists(entry.Models)
	}

	m := newModelTUI(provider, currentModel)
	p := tea.NewProgram(m)
	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	final := finalModel.(modelTUIModel)
	if final.cancelled {
		fmt.Println("Cancelled.")
		return nil
	}

	selectedModel := final.selectedModel()
	if selectedModel == "" {
		return fmt.Errorf("model name cannot be empty")
	}

	if isCustom {
		if cfg.CustomProviders == nil {
			cfg.CustomProviders = make(map[string]ProviderEntry)
		}
		entry := cfg.CustomProviders[cfg.Provider]
		entry.Model = selectedModel
		entry.Models = mergeModelLists([]string{selectedModel}, entry.Models)
		cfg.CustomProviders[cfg.Provider] = entry
	} else {
		if cfg.Providers == nil {
			cfg.Providers = make(map[string]ProviderEntry)
		}
		entry := cfg.Providers[cfg.Provider]
		entry.Model = selectedModel
		if !modelListContains(provider.Models, selectedModel) {
			entry.Models = mergeModelLists([]string{selectedModel}, entry.Models)
		}
		cfg.Providers[cfg.Provider] = entry
	}
	cfg.Model = selectedModel

	if err := saveConfig(configPath, cfg); err != nil {
		return err
	}

	fmt.Printf("\nModel set to: %s\n", selectedModel)
	return nil
}

func saveConfig(path string, cfg *Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod config: %w", err)
	}
	return nil
}

func maskKey(key string) string {
	if key == "" {
		return "(not set)"
	}
	if len(key) <= 8 {
		return "***"
	}
	return key[:4] + "***" + key[len(key)-4:]
}
