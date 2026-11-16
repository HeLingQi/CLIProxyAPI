package pricing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

const BaseLLMPricingSource = "https://basellm.github.io/llm-metadata/api/newapi/ratio_config-v1-base.json"
const maxPricingSourceBytes = 4 << 20

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type SyncTarget struct{ Provider, Model string }
type SyncUnmatched struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}
type SyncSummary struct {
	Source           string          `json:"source"`
	ConfiguredModels int             `json:"configured_models"`
	Matched          int             `json:"matched"`
	Updated          int             `json:"updated"`
	Unmatched        []SyncUnmatched `json:"unmatched"`
}

func LoadBaseLLMModels(ctx context.Context, client HTTPDoer, targets []SyncTarget) ([]ModelConfig, SyncSummary, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, BaseLLMPricingSource, nil)
	if errRequest != nil {
		return nil, SyncSummary{}, errRequest
	}
	response, errDo := client.Do(req)
	if errDo != nil {
		return nil, SyncSummary{}, fmt.Errorf("fetch pricing source: %w", errDo)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, SyncSummary{}, fmt.Errorf("pricing source returned HTTP %d", response.StatusCode)
	}
	body, errRead := io.ReadAll(io.LimitReader(response.Body, maxPricingSourceBytes+1))
	if errRead != nil {
		return nil, SyncSummary{}, fmt.Errorf("read pricing source: %w", errRead)
	}
	if len(body) > maxPricingSourceBytes {
		return nil, SyncSummary{}, errors.New("pricing source exceeds size limit")
	}
	root := ojson.ParseBytes(body)
	if !root.Get("success").Bool() {
		return nil, SyncSummary{}, errors.New("pricing source reported failure")
	}
	expressions := make(map[string]string)
	root.Get("data.billing_expr").ForEach(func(key, value ojson.Result) bool {
		name, expression := strings.TrimSpace(key.String()), strings.TrimSpace(value.String())
		if name != "" && expression != "" {
			expressions[strings.ToLower(name)] = expression
		}
		return true
	})
	if len(expressions) == 0 {
		return nil, SyncSummary{}, errors.New("pricing source contains no expressions")
	}
	for model, expression := range expressions {
		if _, errCompile := compileExpression(expression); errCompile != nil {
			return nil, SyncSummary{}, fmt.Errorf("invalid pricing expression for %s: %w", model, errCompile)
		}
	}
	seen := make(map[modelIdentity]struct{})
	models := make([]ModelConfig, 0, len(targets))
	summary := SyncSummary{Source: BaseLLMPricingSource, Unmatched: []SyncUnmatched{}}
	for _, target := range targets {
		provider, model := normalizeName(target.Provider), strings.TrimSpace(target.Model)
		key := modelKey(provider, model)
		if provider == "" || model == "" {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		summary.ConfiguredModels++
		expression, found := expressions[strings.ToLower(model)]
		if !found {
			summary.Unmatched = append(summary.Unmatched, SyncUnmatched{Provider: provider, Model: model})
			continue
		}
		models = append(models, ModelConfig{Provider: provider, Model: model, Expression: expression, Source: BaseLLMPricingSource})
		summary.Matched++
	}
	summary.Updated = len(models)
	return models, summary, nil
}
