package mock

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhyuzh3d/llmserver/internal/pricing"
	"github.com/zhyuzh3d/llmserver/internal/provider"
)

type Adapter struct {
	ProviderID     string
	ResponseText   string
	ReportedInput  *int64
	ReportedOutput *int64
	DeltaDelay     time.Duration
	Quota          []provider.QuotaObservation
}

func (a *Adapter) ID() string { return a.ProviderID }

func (a *Adapter) Start(ctx context.Context, request provider.Request) (<-chan provider.Event, error) {
	events := make(chan provider.Event)
	go func() {
		defer close(events)
		var built strings.Builder
		for _, delta := range splitRunes(a.ResponseText, 4) {
			if a.DeltaDelay > 0 {
				timer := time.NewTimer(a.DeltaDelay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			built.WriteString(delta)
			select {
			case <-ctx.Done():
				return
			case events <- provider.Event{Type: provider.EventOutputTextDelta, Delta: delta}:
			}
		}

		usage := pricing.ReportedUsage{}
		if a.ReportedInput != nil {
			usage.InputTokens = pricing.OptionalCount{Value: *a.ReportedInput, Present: true}
		}
		if a.ReportedOutput != nil {
			usage.OutputTokens = pricing.OptionalCount{Value: *a.ReportedOutput, Present: true}
		}
		final := &provider.Final{
			OutputText:     built.String(),
			EffectiveModel: request.UpstreamModel,
			Usage:          usage,
			Quota:          append([]provider.QuotaObservation(nil), a.Quota...),
		}
		select {
		case <-ctx.Done():
			return
		case events <- provider.Event{Type: provider.EventCompleted, Final: final}:
		}
	}()
	return events, nil
}

func splitRunes(value string, size int) []string {
	if value == "" {
		return nil
	}
	parts := make([]string, 0, utf8.RuneCountInString(value)/size+1)
	runes := []rune(value)
	for len(runes) > 0 {
		n := size
		if len(runes) < n {
			n = len(runes)
		}
		parts = append(parts, string(runes[:n]))
		runes = runes[n:]
	}
	return parts
}
