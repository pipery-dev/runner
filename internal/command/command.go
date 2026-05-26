package command

import (
	"net/url"
	"strings"
)

type Command struct {
	Name       string
	Properties map[string]string
	Data       string
}

func Parse(line string) (Command, bool) {
	if strings.HasPrefix(line, "::") {
		return parseV2(line)
	}
	if strings.HasPrefix(line, "##[") {
		return parseLegacy(line)
	}
	return Command{}, false
}

func parseV2(line string) (Command, bool) {
	body := strings.TrimPrefix(line, "::")
	idx := strings.Index(body, "::")
	if idx < 0 {
		return Command{}, false
	}

	head := body[:idx]
	data := unescapeData(body[idx+2:])
	name, props := parseHead(head)
	if name == "" {
		return Command{}, false
	}
	return Command{Name: strings.ToLower(name), Properties: props, Data: data}, true
}

func parseLegacy(line string) (Command, bool) {
	end := strings.Index(line, "]")
	if end < 3 {
		return Command{}, false
	}
	head := line[3:end]
	name, props := parseHead(head)
	if name == "" {
		return Command{}, false
	}
	return Command{Name: strings.ToLower(name), Properties: props, Data: line[end+1:]}, true
}

func parseHead(head string) (string, map[string]string) {
	parts := strings.Split(head, " ")
	name := strings.TrimSpace(parts[0])
	props := map[string]string{}
	if len(parts) == 1 {
		return name, props
	}

	for _, part := range strings.Split(strings.Join(parts[1:], " "), ",") {
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		props[strings.TrimSpace(key)] = unescapeProperty(strings.TrimSpace(value))
	}
	return name, props
}

func unescapeData(s string) string {
	replacer := strings.NewReplacer("%0D", "\r", "%0A", "\n", "%25", "%")
	return replacer.Replace(s)
}

func unescapeProperty(s string) string {
	replacer := strings.NewReplacer("%0D", "\r", "%0A", "\n", "%3A", ":", "%2C", ",", "%25", "%")
	return replacer.Replace(s)
}

func EscapeData(s string) string {
	replacer := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")
	return replacer.Replace(s)
}

func EscapeProperty(s string) string {
	return url.QueryEscape(s)
}
