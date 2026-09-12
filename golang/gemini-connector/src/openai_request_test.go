package main

import (
	"testing"
)

func TestFieldPolicy(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		expectErr   bool
		errCode     string
		expectModel string
		expectStop  int
	}{
		{
			name:        "Valid minimal request",
			body:        `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hello"}]}`,
			expectErr:   false,
			expectModel: "gemini-3.8-flash-high",
		},
		{
			name:      "Missing model",
			body:      `{"messages":[{"role":"user","content":"hello"}]}`,
			expectErr: true,
			errCode:   "missing_required_parameter",
		},
		{
			name:      "Empty messages",
			body:      `{"model":"gemini-3.8-flash-high","messages":[]}`,
			expectErr: true,
			errCode:   "missing_required_parameter",
		},
		{
			name:      "Rejected field audio",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"audio":{}}`,
			expectErr: true,
			errCode:   "unsupported_field",
		},
		{
			name:      "Rejected field modalities",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"modalities":["text"]}`,
			expectErr: true,
			errCode:   "unsupported_field",
		},
		{
			name:      "logprobs true rejected",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"logprobs":true}`,
			expectErr: true,
			errCode:   "unsupported_parameter",
		},
		{
			name:      "logprobs false accepted",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"logprobs":false}`,
			expectErr: false,
		},
		{
			name:      "n=2 rejected",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"n":2}`,
			expectErr: true,
			errCode:   "unsupported_value",
		},
		{
			name:      "n=0 rejected",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"n":0}`,
			expectErr: true,
			errCode:   "unsupported_value",
		},
		{
			name:      "n=1 accepted",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"n":1}`,
			expectErr: false,
		},
		{
			name:      "max_tokens negative rejected",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"max_tokens":-5}`,
			expectErr: true,
			errCode:   "invalid_value",
		},
		{
			name:      "max_tokens and max_completion_tokens precedence accepted",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"max_completion_tokens":200}`,
			expectErr: false,
		},
		{
			name:      "max_tokens and max_completion_tokens match accepted",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"max_completion_tokens":100}`,
			expectErr: false,
		},
		{
			name:        "stop string normalized",
			body:        `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"stop":"<END>"}`,
			expectErr:   false,
			expectStop:  1,
		},
		{
			name:        "stop array normalized",
			body:        `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"stop":["<END1>","<END2>"]}`,
			expectErr:   false,
			expectStop:  2,
		},
		{
			name:      "stop array exceeding 4 rejected",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"stop":["1","2","3","4","5"]}`,
			expectErr: true,
			errCode:   "invalid_value",
		},
		{
			name:      "tool without function definition rejected",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom"}]}`,
			expectErr: true,
			errCode:   "unsupported_tool_type",
		},
		{
			name:      "duplicate tool name rejected",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"foo"}},{"type":"function","function":{"name":"foo"}}]}`,
			expectErr: true,
			errCode:   "duplicate_tool_name",
		},
		{
			name:      "tool function strict:true rejected",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"foo","strict":true}}]}`,
			expectErr: true,
			errCode:   "unsupported_parameter",
		},
		{
			name:      "tool_choice without tools rejected",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"tool_choice":"required"}`,
			expectErr: true,
			errCode:   "invalid_parameter_combination",
		},
		{
			name:      "tool_choice none without tools accepted",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"tool_choice":"none"}`,
			expectErr: false,
		},
		{
			name:      "tool_choice unknown function rejected",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"bar"}}],"tool_choice":{"type":"function","function":{"name":"foo"}}}`,
			expectErr: true,
			errCode:   "invalid_tool_choice",
		},
		{
			name:      "tools combined with json_object response_format rejected",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"foo"}}],"response_format":{"type":"json_object"}}`,
			expectErr: true,
			errCode:   "unsupported_combination",
		},
		{
			name:      "response_format json_schema with strict:true rejected",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"test","strict":true}}}`,
			expectErr: true,
			errCode:   "unsupported_parameter",
		},
		{
			name:      "temperature out of range rejected",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"temperature":2.5}`,
			expectErr: true,
			errCode:   "invalid_value",
		},
		{
			name:      "temperature valid accepted",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":"hi"}],"temperature":0.7}`,
			expectErr: false,
		},
		{
			name:      "tool message missing tool_call_id rejected",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"tool","content":"ok"}]}`,
			expectErr: true,
			errCode:   "missing_tool_call_id",
		},
		{
			name:      "unsupported content block type rejected",
			body:      `{"model":"gemini-3.8-flash-high","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com"}}]}]}`,
			expectErr: true,
			errCode:   "unsupported_content_type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			norm, valErr := ParseAndNormalizeRequest([]byte(tt.body))
			if tt.expectErr {
				if valErr == nil {
					t.Fatalf("expected error but got none")
				}
				if tt.errCode != "" && valErr.Code != tt.errCode {
					t.Errorf("expected error code %q, got %q (msg: %s)", tt.errCode, valErr.Code, valErr.Message)
				}
			} else {
				if valErr != nil {
					t.Fatalf("unexpected error: %v (code: %s, param: %v)", valErr, valErr.Code, valErr.Param)
				}
				if tt.expectModel != "" && norm.Model != tt.expectModel {
					t.Errorf("expected model %q, got %q", tt.expectModel, norm.Model)
				}
				if tt.expectStop > 0 && len(norm.Stop) != tt.expectStop {
					t.Errorf("expected %d stop items, got %d", tt.expectStop, len(norm.Stop))
				}
			}
		})
	}
}
