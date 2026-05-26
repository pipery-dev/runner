package job

import (
	"encoding/json"
	"fmt"
	"strings"
)

type Message struct {
	MessageType    string                   `json:"messageType,omitempty"`
	RequestID      int64                    `json:"requestId,omitempty"`
	JobID          string                   `json:"jobId,omitempty"`
	JobDisplayName string                   `json:"jobDisplayName,omitempty"`
	JobName        string                   `json:"jobName,omitempty"`
	Variables      map[string]VariableValue `json:"variables,omitempty"`
	MaskHints      []MaskHint               `json:"maskHints,omitempty"`
	Steps          []Step                   `json:"steps,omitempty"`
}

type VariableValue struct {
	Value    string `json:"value,omitempty"`
	IsSecret bool   `json:"isSecret,omitempty"`
}

type MaskHint struct {
	Type  string `json:"type,omitempty"`
	Value string `json:"value,omitempty"`
}

type Step struct {
	ID          string            `json:"id,omitempty"`
	DisplayName string            `json:"displayName,omitempty"`
	ContextName string            `json:"contextName,omitempty"`
	Type        string            `json:"type,omitempty"`
	Inputs      map[string]string `json:"inputs,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}

func (s *Step) UnmarshalJSON(data []byte) error {
	type alias Step
	var raw struct {
		alias
		Inputs      json.RawMessage `json:"inputs"`
		Environment json.RawMessage `json:"environment"`
		Env         json.RawMessage `json:"env"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	*s = Step(raw.alias)
	var err error
	if s.Inputs, err = decodeStringMap(raw.Inputs); err != nil {
		return fmt.Errorf("inputs: %w", err)
	}
	if s.Environment, err = decodeStringMap(raw.Environment); err != nil {
		return fmt.Errorf("environment: %w", err)
	}
	if s.Env, err = decodeStringMap(raw.Env); err != nil {
		return fmt.Errorf("env: %w", err)
	}
	return nil
}

func (s Step) Name() string {
	if s.DisplayName != "" {
		return s.DisplayName
	}
	if s.ContextName != "" {
		return s.ContextName
	}
	if s.ID != "" {
		return s.ID
	}
	return "step"
}

func (s Step) Script() string {
	return s.Inputs["script"]
}

func (s Step) Shell() string {
	return s.Inputs["shell"]
}

func (s Step) WorkingDirectory() string {
	if wd := s.Inputs["workingDirectory"]; wd != "" {
		return wd
	}
	return s.Inputs["working-directory"]
}

func (s Step) MergedEnv() map[string]string {
	out := make(map[string]string, len(s.Environment)+len(s.Env))
	for k, v := range s.Environment {
		out[k] = v
	}
	for k, v := range s.Env {
		out[k] = v
	}
	return out
}

func decodeStringMap(data json.RawMessage) (map[string]string, error) {
	if len(data) == 0 || string(data) == "null" {
		return map[string]string{}, nil
	}

	var values map[string]string
	if err := json.Unmarshal(data, &values); err == nil {
		if values == nil {
			values = map[string]string{}
		}
		return values, nil
	}

	var anyValues map[string]any
	if err := json.Unmarshal(data, &anyValues); err != nil {
		return nil, err
	}
	values = make(map[string]string, len(anyValues))
	for k, v := range anyValues {
		switch x := v.(type) {
		case nil:
			values[k] = ""
		case string:
			values[k] = x
		default:
			values[k] = strings.TrimSpace(fmt.Sprint(x))
		}
	}
	return values, nil
}
