package pricing

import (
	"errors"
	"fmt"
	"math/big"
)

const perMillion = int64(1_000_000)

type UsageSource string

const (
	UsageProviderReported UsageSource = "provider_reported"
	UsageEstimatedV1      UsageSource = "estimated_v1"
)

type OptionalCount struct {
	Value   int64
	Present bool
}

type ReportedUsage struct {
	InputTokens  OptionalCount
	OutputTokens OptionalCount
}

type BillableUsage struct {
	InputTokens    int64
	OutputTokens   int64
	InputSource    UsageSource
	OutputSource   UsageSource
	InputEstimate  *Estimate
	OutputEstimate *Estimate
}

type PriceRevision struct {
	ID               string
	Currency         string
	InputPerMillion  Decimal
	OutputPerMillion Decimal
}

type Settlement struct {
	PriceRevision string
	Currency      string
	Usage         BillableUsage
	InputCharge   Decimal
	OutputCharge  Decimal
	TotalCharge   Decimal
}

func ResolveUsage(reported ReportedUsage, inputText, outputText string) (BillableUsage, error) {
	usage := BillableUsage{}
	if reported.InputTokens.Present {
		if reported.InputTokens.Value < 0 {
			return BillableUsage{}, errors.New("reported input tokens cannot be negative")
		}
		usage.InputTokens = reported.InputTokens.Value
		usage.InputSource = UsageProviderReported
	} else {
		estimate := EstimateTextV1(inputText)
		usage.InputTokens = estimate.Tokens
		usage.InputSource = UsageEstimatedV1
		usage.InputEstimate = &estimate
	}

	if reported.OutputTokens.Present {
		if reported.OutputTokens.Value < 0 {
			return BillableUsage{}, errors.New("reported output tokens cannot be negative")
		}
		usage.OutputTokens = reported.OutputTokens.Value
		usage.OutputSource = UsageProviderReported
	} else {
		estimate := EstimateTextV1(outputText)
		usage.OutputTokens = estimate.Tokens
		usage.OutputSource = UsageEstimatedV1
		usage.OutputEstimate = &estimate
	}
	return usage, nil
}

func Calculate(price PriceRevision, usage BillableUsage) (Settlement, error) {
	if price.ID == "" || price.Currency == "" {
		return Settlement{}, errors.New("price revision id and currency are required")
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return Settlement{}, errors.New("billable usage cannot be negative")
	}
	inputCharge, err := multiplyTokens(usage.InputTokens, price.InputPerMillion)
	if err != nil {
		return Settlement{}, fmt.Errorf("calculate input charge: %w", err)
	}
	outputCharge, err := multiplyTokens(usage.OutputTokens, price.OutputPerMillion)
	if err != nil {
		return Settlement{}, fmt.Errorf("calculate output charge: %w", err)
	}
	total, err := inputCharge.Add(outputCharge)
	if err != nil {
		return Settlement{}, fmt.Errorf("calculate total charge: %w", err)
	}
	return Settlement{
		PriceRevision: price.ID,
		Currency:      price.Currency,
		Usage:         usage,
		InputCharge:   inputCharge,
		OutputCharge:  outputCharge,
		TotalCharge:   total,
	}, nil
}

func multiplyTokens(tokens int64, rate Decimal) (Decimal, error) {
	if tokens < 0 {
		return Decimal{}, errors.New("tokens cannot be negative")
	}
	product := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(rate.Nanos()))
	// Round half up to the nearest currency nano-unit.
	product.Add(product, big.NewInt(perMillion/2))
	product.Div(product, big.NewInt(perMillion))
	if !product.IsInt64() {
		return Decimal{}, errors.New("charge overflows")
	}
	return DecimalFromNanos(product.Int64())
}

type BudgetMode string

const (
	BudgetHard BudgetMode = "hard"
	BudgetSoft BudgetMode = "soft"
)

type Budget struct {
	Mode      BudgetMode
	Currency  string
	MaxCharge Decimal
}

type BudgetEvaluation struct {
	Allowed               bool
	MaximumPossibleCharge Decimal
	Reason                string
}

func EvaluateHardBudget(price PriceRevision, inputTokens, maxOutputTokens int64, budget Budget) (BudgetEvaluation, error) {
	if budget.Mode != BudgetHard {
		return BudgetEvaluation{}, errors.New("hard budget evaluation requires hard mode")
	}
	if budget.Currency != price.Currency {
		return BudgetEvaluation{}, errors.New("budget currency does not match price currency")
	}
	if inputTokens < 0 || maxOutputTokens < 0 {
		return BudgetEvaluation{}, errors.New("budget token bounds cannot be negative")
	}
	usage := BillableUsage{InputTokens: inputTokens, OutputTokens: maxOutputTokens}
	settlement, err := Calculate(price, usage)
	if err != nil {
		return BudgetEvaluation{}, err
	}
	allowed := settlement.TotalCharge.Nanos() <= budget.MaxCharge.Nanos()
	reason := ""
	if !allowed {
		reason = "budget_exceeded_before_start"
	}
	return BudgetEvaluation{
		Allowed:               allowed,
		MaximumPossibleCharge: settlement.TotalCharge,
		Reason:                reason,
	}, nil
}
