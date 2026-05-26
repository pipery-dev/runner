package masker

import (
	"regexp"
	"sort"
	"strings"
)

type Masker struct {
	values  []string
	regexps []*regexp.Regexp
}

func New() *Masker {
	return &Masker{}
}

func (m *Masker) AddValue(value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	for _, existing := range m.values {
		if existing == value {
			return
		}
	}
	m.values = append(m.values, value)
	sort.SliceStable(m.values, func(i, j int) bool {
		return len(m.values[i]) > len(m.values[j])
	})
}

func (m *Masker) AddRegex(pattern string) error {
	if pattern == "" {
		return nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return err
	}
	m.regexps = append(m.regexps, re)
	return nil
}

func (m *Masker) Mask(line string) string {
	for _, value := range m.values {
		line = strings.ReplaceAll(line, value, "***")
	}
	for _, re := range m.regexps {
		line = re.ReplaceAllString(line, "***")
	}
	return line
}
