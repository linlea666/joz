package copytrader

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var reductionClause = regexp.MustCompile(`(?i)(減倉|减仓|減掉|减掉|減半|减半|平倉|平仓|止盈|\breduce\b|\btrim\b|\bclose\b|\btake\b)`)
var ratioClauses = regexp.MustCompile(`[\n，,;；。]`)
var ratioRange = regexp.MustCompile(`(?i)[0-9]+(?:\.[0-9]+)?\s*[%％]?\s*(?:[～~—–-]|to|到|至)\s*[0-9]+(?:\.[0-9]+)?\s*[%％]`)
var fractionRange = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)?\s*/\s*[0-9]+(?:\.[0-9]+)?\s*(?:[～~—–-]|to|到|至)\s*[0-9]+(?:\.[0-9]+)?\s*/\s*[0-9]+`)
var chineseRatioRange = regexp.MustCompile(`(?:百分之)?[零一二两兩三四五六七八九十百]+(?:成半?|分之[零一二两兩三四五六七八九十百]+)?\s*[～~—–到至-]\s*(?:百分之)?[零一二两兩三四五六七八九十百]+(?:成半?|分之[零一二两兩三四五六七八九十百]+)?`)
var percentRatio = regexp.MustCompile(`(-?[0-9]+(?:\.[0-9]+)?)\s*[%％]`)
var numericFraction = regexp.MustCompile(`(-?[0-9]+(?:\.[0-9]+)?)\s*/\s*(-?[0-9]+(?:\.[0-9]+)?)`)
var chinesePercent = regexp.MustCompile(`百分之([零一二两兩三四五六七八九十百]+)`)
var chineseTenths = regexp.MustCompile(`([一二两兩三四五六七八九十])成(半)?`)
var chineseFraction = regexp.MustCompile(`([零一二两兩三四五六七八九十百]+)分之([零一二两兩三四五六七八九十百]+)`)
var ratioWords = regexp.MustCompile(`(?i)(一半|減半|减半|\bhalf\b|\bquarter\b|\bthird\b)`)

// Both deterministic and model-backed paths use author evidence, scoped to
// reduction clauses, so leverage, returns and position-sizing examples cannot
// become a close percentage. Conflicting values never silently choose one.
func explicitReductionRatio(scope string) (float64, bool, error) {
	var values []float64
	for _, clause := range ratioClauses.Split(scope, -1) {
		if !reductionClause.MatchString(clause) {
			continue
		}
		if ratioRange.MatchString(clause) || fractionRange.MatchString(clause) || chineseRatioRange.MatchString(clause) {
			return 0, true, fmt.Errorf("reduction ratio is a range")
		}
		for _, m := range percentRatio.FindAllStringSubmatch(clause, -1) {
			v, _ := strconv.ParseFloat(m[1], 64)
			values = append(values, v)
		}
		for _, m := range numericFraction.FindAllStringSubmatch(clause, -1) {
			n, _ := strconv.ParseFloat(m[1], 64)
			d, _ := strconv.ParseFloat(m[2], 64)
			if d <= 0 {
				return 0, true, fmt.Errorf("reduction fraction has invalid denominator")
			}
			values = append(values, n/d*100)
		}
		for _, m := range chinesePercent.FindAllStringSubmatch(clause, -1) {
			n, ok := smallChineseNumber(m[1])
			if !ok {
				return 0, true, fmt.Errorf("unsupported Chinese reduction percentage")
			}
			values = append(values, n)
		}
		for _, m := range chineseTenths.FindAllStringSubmatch(clause, -1) {
			n, _ := smallChineseNumber(m[1])
			n *= 10
			if m[2] != "" {
				n += 5
			}
			values = append(values, n)
		}
		for _, m := range chineseFraction.FindAllStringSubmatch(clause, -1) {
			n, nok := smallChineseNumber(m[2])
			d, dok := smallChineseNumber(m[1])
			if !nok || !dok || d <= 0 {
				return 0, true, fmt.Errorf("invalid Chinese reduction fraction")
			}
			values = append(values, n/d*100)
		}
		for _, m := range ratioWords.FindAllString(clause, -1) {
			v := 50.0
			switch strings.ToLower(m) {
			case "quarter":
				v = 25
			case "third":
				v = 100.0 / 3
			}
			values = append(values, v)
		}
		// Unknown fraction spellings must not fall back to the default.
		if strings.Contains(clause, "分之") && !chineseFraction.MatchString(clause) && !chinesePercent.MatchString(clause) {
			return 0, true, fmt.Errorf("unsupported reduction fraction")
		}
	}
	if len(values) == 0 {
		return 0, false, nil
	}
	v := values[0]
	for _, n := range values {
		if !finite(n) || n <= 0 || n > 100 || math.Abs(n-v) > 1e-8 {
			return 0, true, fmt.Errorf("invalid or conflicting explicit reduction ratios")
		}
	}
	return v, true, nil
}

func smallChineseNumber(s string) (float64, bool) {
	s = strings.NewReplacer("两", "二", "兩", "二").Replace(s)
	if s == "一百" || s == "百" {
		return 100, true
	}
	digit := func(s string) (int, bool) {
		for i, r := range []rune("零一二三四五六七八九") {
			if s == string(r) {
				return i, true
			}
		}
		return 0, false
	}
	if n, ok := digit(s); ok {
		return float64(n), true
	}
	parts := strings.Split(s, "十")
	if len(parts) != 2 {
		return 0, false
	}
	tens, units := 1, 0
	var ok bool
	if parts[0] != "" {
		tens, ok = digit(parts[0])
		if !ok || tens == 0 {
			return 0, false
		}
	}
	if parts[1] != "" {
		units, ok = digit(parts[1])
		if !ok || units == 0 {
			return 0, false
		}
	}
	return float64(tens*10 + units), true
}

func applyReductionRatio(ins *SourceInterpretation, scope string, sources []SourceSegment, fallback float64) {
	if ins.ActionEvidence != nil {
		if source := findSourceSegment(sources, ins.ActionEvidence.SourceID); source != nil && source.Image {
			if ins.CloseRatio == nil {
				ins.CloseRatio = &fallback
			}
			return
		}
	}
	v, explicit, err := explicitReductionRatio(scope)
	if err != nil {
		ins.Classification = ClassificationAmbiguous
		ins.Reasoning = err.Error()
		return
	}
	if !explicit {
		v = fallback
	}
	ins.CloseRatio = &v
}
