package transfer

// 分类规则匹配模块（对应 category_helper.py）。
// 解析 config/category.yaml，按 genre_ids / original_language / origin_country 等匹配分类。

import (
	"log"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// categoryRule 单个分类规则，顺序即 yaml 定义顺序（分类按先后顺序匹配）。
type categoryRule struct {
	Name string
	Cond map[string]any // nil 表示无条件（兜底）分类
}

// CategoryHelper 分类规则匹配器。
type CategoryHelper struct {
	YAMLPath string
	rules    map[string][]categoryRule
}

// NewCategoryHelper 加载分类规则。
func NewCategoryHelper(yamlPath string) *CategoryHelper {
	h := &CategoryHelper{YAMLPath: yamlPath}
	h.load()
	return h
}

func (h *CategoryHelper) load() {
	data, err := os.ReadFile(h.YAMLPath)
	if err != nil {
		log.Printf("分类规则文件不存在: %s", h.YAMLPath)
		return
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		log.Printf("加载分类规则失败 (%s): %v", h.YAMLPath, err)
		return
	}
	doc := root.Content[0] // 文档根映射
	if doc == nil || doc.Kind != yaml.MappingNode {
		log.Printf("分类规则格式错误 (非映射): %s", h.YAMLPath)
		return
	}
	h.rules = make(map[string][]categoryRule)
	for i := 0; i+1 < len(doc.Content); i += 2 {
		mtype := doc.Content[i].Value
		sec := doc.Content[i+1]
		if sec.Kind != yaml.MappingNode {
			continue
		}
		var rules []categoryRule
		for j := 0; j+1 < len(sec.Content); j += 2 {
			catName := sec.Content[j].Value
			condNode := sec.Content[j+1]
			var cond map[string]any
			if condNode.Kind == yaml.ScalarNode && condNode.Tag == "!!null" {
				cond = nil
			} else if condNode.Kind == yaml.MappingNode {
				_ = condNode.Decode(&cond)
			}
			rules = append(rules, categoryRule{Name: catName, Cond: cond})
		}
		h.rules[mtype] = rules
	}
	log.Printf("✅ 已加载分类规则: %s", h.YAMLPath)
}

// Match 按 media.type 取 movie/tv 节，按 yaml 定义顺序匹配分类条件。
// 匹配失败则返回兜底分类（无条件项）；无规则返回空字符串。
func (h *CategoryHelper) Match(media *MediaInfo) string {
	if media == nil || media.Type == "" {
		return ""
	}
	rules, ok := h.rules[media.Type]
	if !ok || len(rules) == 0 {
		return ""
	}
	// 无条件（兜底）分类放最后，先试条件分类
	var fallback string
	for _, rule := range rules {
		if len(rule.Cond) == 0 {
			fallback = rule.Name
			continue
		}
		if matchConditions(media, rule.Cond) {
			return rule.Name
		}
	}
	if fallback != "" {
		return fallback
	}
	return rules[0].Name
}

// matchConditions 检查 media 是否满足所有条件（AND 关系）。
func matchConditions(media *MediaInfo, conditions map[string]any) bool {
	for field, expected := range conditions {
		exp := strval(expected)
		if !matchOne(media, field, exp) {
			return false
		}
	}
	return true
}

// matchOne 检查单个条件。
func matchOne(media *MediaInfo, field, expected string) bool {
	// 解析期望值
	negate := false
	if strings.HasPrefix(expected, "!") {
		negate = true
		expected = expected[1:]
	}
	var expectedList []string
	for _, v := range strings.Split(expected, ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			expectedList = append(expectedList, v)
		}
	}

	// 获取 media 的实际值
	var actualValues []string
	switch field {
	case "genre_ids":
		for _, g := range media.GenreIDs {
			actualValues = append(actualValues, strconv.Itoa(g))
		}
	case "original_language":
		if media.OriginalLanguage != "" {
			actualValues = append(actualValues, media.OriginalLanguage)
		}
	case "origin_country":
		actualValues = media.OriginCountry
	case "production_countries":
		actualValues = media.ProductionCountries
	case "release_year":
		if media.Year > 0 {
			actualValues = append(actualValues, strconv.Itoa(media.Year))
		}
	}

	// 匹配逻辑
	matched := false
	if field == "release_year" {
		// 年份支持范围：YYYY-YYYY
		for _, exp := range expectedList {
			if strings.Contains(exp, "-") && len(strings.Split(exp, "-")) == 2 {
				parts := strings.Split(exp, "-")
				lo, err1 := strconv.Atoi(parts[0])
				hi, err2 := strconv.Atoi(parts[1])
				if err1 == nil && err2 == nil && len(actualValues) > 0 {
					av, _ := strconv.Atoi(actualValues[0])
					if lo <= av && av <= hi {
						matched = true
						break
					}
				}
			} else {
				if containsStr(actualValues, exp) {
					matched = true
					break
				}
			}
		}
	} else {
		matched = containsAny(actualValues, expectedList)
	}

	if negate {
		return !matched
	}
	return matched
}

// ListCategories 按 yaml 定义顺序列出某类型的所有分类名。
func (h *CategoryHelper) ListCategories(mtype string) []string {
	rules, ok := h.rules[mtype]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(rules))
	for _, rule := range rules {
		out = append(out, rule.Name)
	}
	return out
}

func strval(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, strval(e))
		}
		return strings.Join(parts, ",")
	}
	return ""
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func containsAny(actual, expected []string) bool {
	for _, a := range actual {
		for _, e := range expected {
			if a == e {
				return true
			}
		}
	}
	return false
}
