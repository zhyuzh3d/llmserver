package pricing

import "unicode"

const TextEstimatorV1 = "text_estimator_v1"

type Estimate struct {
	Tokens     int64
	Characters int64
	CJK        int64
	Other      int64
	Version    string
}

func EstimateTextV1(text string) Estimate {
	var cjk, other int64
	for _, r := range text {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	tokens := cjk + ceilDiv(other, 4)
	if text != "" && tokens == 0 {
		tokens = 1
	}
	return Estimate{
		Tokens:     tokens,
		Characters: cjk + other,
		CJK:        cjk,
		Other:      other,
		Version:    TextEstimatorV1,
	}
}

func isCJK(r rune) bool {
	return unicode.In(r,
		unicode.Han,
		unicode.Hiragana,
		unicode.Katakana,
		unicode.Hangul,
		unicode.Bopomofo,
	)
}

func ceilDiv(value, divisor int64) int64 {
	if value == 0 {
		return 0
	}
	return (value + divisor - 1) / divisor
}
