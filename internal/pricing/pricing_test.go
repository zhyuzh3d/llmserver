package pricing

import "testing"

func TestEstimateTextV1(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int64
	}{
		{name: "empty", text: "", want: 0},
		{name: "english", text: "abcdefgh", want: 2},
		{name: "chinese", text: "你好世界", want: 4},
		{name: "mixed", text: "你好abcd", want: 3},
		{name: "short non-cjk", text: "a", want: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := EstimateTextV1(test.text).Tokens; got != test.want {
				t.Fatalf("EstimateTextV1(%q) = %d, want %d", test.text, got, test.want)
			}
		})
	}
}

func TestResolveUsageEstimatesOnlyMissingDimension(t *testing.T) {
	usage, err := ResolveUsage(ReportedUsage{
		InputTokens: OptionalCount{Value: 123, Present: true},
	}, "ignored input", "你好abcd")
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 123 || usage.InputSource != UsageProviderReported {
		t.Fatalf("reported input was not preserved: %#v", usage)
	}
	if usage.OutputTokens != 3 || usage.OutputSource != UsageEstimatedV1 || usage.OutputEstimate == nil {
		t.Fatalf("missing output was not estimated: %#v", usage)
	}
}

func TestCalculateUsesConfiguredInputAndOutputPrice(t *testing.T) {
	inputRate := mustDecimal(t, "2")
	outputRate := mustDecimal(t, "12")
	settlement, err := Calculate(PriceRevision{
		ID:               "price-v1",
		Currency:         "USD",
		InputPerMillion:  inputRate,
		OutputPerMillion: outputRate,
	}, BillableUsage{InputTokens: 2000, OutputTokens: 300})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := settlement.InputCharge.String(), "0.004000000"; got != want {
		t.Fatalf("input charge = %s, want %s", got, want)
	}
	if got, want := settlement.OutputCharge.String(), "0.003600000"; got != want {
		t.Fatalf("output charge = %s, want %s", got, want)
	}
	if got, want := settlement.TotalCharge.String(), "0.007600000"; got != want {
		t.Fatalf("total charge = %s, want %s", got, want)
	}
}

func TestSmallChargesDoNotRoundToZeroAtSixDecimals(t *testing.T) {
	settlement, err := Calculate(PriceRevision{
		ID:               "luna",
		Currency:         "USD",
		InputPerMillion:  mustDecimal(t, "0.2"),
		OutputPerMillion: mustDecimal(t, "1.2"),
	}, BillableUsage{InputTokens: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := settlement.InputCharge.String(), "0.000000200"; got != want {
		t.Fatalf("small charge = %s, want %s", got, want)
	}
}

func TestEvaluateHardBudget(t *testing.T) {
	price := PriceRevision{
		ID:               "price-v1",
		Currency:         "USD",
		InputPerMillion:  mustDecimal(t, "2"),
		OutputPerMillion: mustDecimal(t, "12"),
	}
	evaluation, err := EvaluateHardBudget(price, 2000, 300, Budget{
		Mode:      BudgetHard,
		Currency:  "USD",
		MaxCharge: mustDecimal(t, "0.007599999"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Allowed || evaluation.Reason != "budget_exceeded_before_start" {
		t.Fatalf("expected rejected budget, got %#v", evaluation)
	}
}

func TestParseDecimalRejectsExcessPrecisionAndNegative(t *testing.T) {
	for _, value := range []string{"-1", "1.1234567890", "abc"} {
		if _, err := ParseDecimal(value); err == nil {
			t.Fatalf("ParseDecimal(%q) unexpectedly succeeded", value)
		}
	}
}

func mustDecimal(t *testing.T, value string) Decimal {
	t.Helper()
	decimal, err := ParseDecimal(value)
	if err != nil {
		t.Fatal(err)
	}
	return decimal
}
