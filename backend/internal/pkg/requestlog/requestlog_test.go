package requestlog

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSimplifyAnthropicRequest(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantJSON string // 空表示期望返回空串
	}{
		{
			name: "first turn user text only",
			body: `{
				"model": "claude-3-5-sonnet",
				"messages": [
					{"role": "user", "content": "hello"}
				]
			}`,
			wantJSON: `{"model":"claude-3-5-sonnet","user_input":"hello"}`,
		},
		{
			name: "user content array with text block",
			body: `{
				"messages": [
					{"role": "user", "content": [{"type":"text","text":"ping"}]}
				]
			}`,
			wantJSON: `{"user_input":"ping"}`,
		},
		{
			name: "single tool round",
			body: `{
				"model": "claude-x",
				"messages": [
					{"role": "user", "content": "find file"},
					{"role": "assistant", "content": [
						{"type":"tool_use","id":"tu_1","name":"bash","input":{"cmd":"ls"}}
					]},
					{"role": "user", "content": [
						{"type":"tool_result","tool_use_id":"tu_1","content":"file1\nfile2"}
					]}
				]
			}`,
			wantJSON: `{
				"model":"claude-x",
				"user_input":"find file",
				"tool_uses":[{"id":"tu_1","name":"bash","input":{"cmd":"ls"}}],
				"tool_results":[{"tool_use_id":"tu_1","content":"file1\nfile2"}]
			}`,
		},
		{
			name: "parallel tool uses",
			body: `{
				"messages": [
					{"role":"user","content":"do two things"},
					{"role":"assistant","content":[
						{"type":"tool_use","id":"tu_1","name":"read","input":{"path":"a"}},
						{"type":"tool_use","id":"tu_2","name":"read","input":{"path":"b"}}
					]},
					{"role":"user","content":[
						{"type":"tool_result","tool_use_id":"tu_1","content":"A"},
						{"type":"tool_result","tool_use_id":"tu_2","content":"B"}
					]}
				]
			}`,
			wantJSON: `{
				"user_input":"do two things",
				"tool_uses":[
					{"id":"tu_1","name":"read","input":{"path":"a"}},
					{"id":"tu_2","name":"read","input":{"path":"b"}}
				],
				"tool_results":[
					{"tool_use_id":"tu_1","content":"A"},
					{"tool_use_id":"tu_2","content":"B"}
				]
			}`,
		},
		{
			name: "multi turn keeps only last group",
			body: `{
				"messages": [
					{"role":"user","content":"first task"},
					{"role":"assistant","content":[
						{"type":"tool_use","id":"tu_a","name":"read","input":{}}
					]},
					{"role":"user","content":[
						{"type":"tool_result","tool_use_id":"tu_a","content":"X"}
					]},
					{"role":"assistant","content":[
						{"type":"tool_use","id":"tu_b","name":"edit","input":{"file":"f"}}
					]},
					{"role":"user","content":[
						{"type":"tool_result","tool_use_id":"tu_b","content":"Y"}
					]}
				]
			}`,
			wantJSON: `{
				"user_input":"first task",
				"tool_uses":[{"id":"tu_b","name":"edit","input":{"file":"f"}}],
				"tool_results":[{"tool_use_id":"tu_b","content":"Y"}]
			}`,
		},
		{
			name: "user_input falls back when last user is pure tool_result",
			body: `{
				"messages": [
					{"role":"user","content":"原始任务"},
					{"role":"assistant","content":[
						{"type":"tool_use","id":"tu_1","name":"bash","input":{}}
					]},
					{"role":"user","content":[
						{"type":"tool_result","tool_use_id":"tu_1","content":"OK"}
					]}
				]
			}`,
			wantJSON: `{
				"user_input":"原始任务",
				"tool_uses":[{"id":"tu_1","name":"bash","input":{}}],
				"tool_results":[{"tool_use_id":"tu_1","content":"OK"}]
			}`,
		},
		{
			name: "ChatCompletions shape returns empty",
			body: `{
				"messages": [
					{"role":"user","content":"hi"},
					{"role":"assistant","tool_calls":[
						{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}
					]},
					{"role":"tool","tool_call_id":"call_1","content":"result"}
				]
			}`,
			wantJSON: "",
		},
		{name: "no messages key", body: `{"model":"x"}`, wantJSON: ""},
		{name: "empty messages", body: `{"messages":[]}`, wantJSON: ""},
		{name: "non-JSON", body: `not json`, wantJSON: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SimplifyAnthropicRequest([]byte(tc.body))
			if tc.wantJSON == "" {
				assert.Empty(t, got)
				return
			}
			require.NotEmpty(t, got)
			assert.JSONEq(t, tc.wantJSON, got)
		})
	}
}

func TestSimplifyChatCompletionsRequest(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantJSON string
	}{
		{
			name:     "first turn",
			body:     `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`,
			wantJSON: `{"model":"gpt-4","user_input":"hi"}`,
		},
		{
			name: "single tool round with string-form arguments",
			body: `{
				"messages": [
					{"role":"user","content":"用 search 工具"},
					{"role":"assistant","content":null,"tool_calls":[
						{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"hello\"}"}}
					]},
					{"role":"tool","tool_call_id":"call_1","content":"found 3 results"}
				]
			}`,
			wantJSON: `{
				"user_input":"用 search 工具",
				"tool_uses":[{"id":"call_1","name":"search","input":{"q":"hello"}}],
				"tool_results":[{"tool_use_id":"call_1","content":"found 3 results"}]
			}`,
		},
		{
			name: "parallel tool calls",
			body: `{
				"messages": [
					{"role":"user","content":"并行查"},
					{"role":"assistant","content":null,"tool_calls":[
						{"id":"c1","type":"function","function":{"name":"f1","arguments":"{}"}},
						{"id":"c2","type":"function","function":{"name":"f2","arguments":"{\"x\":1}"}}
					]},
					{"role":"tool","tool_call_id":"c1","content":"r1"},
					{"role":"tool","tool_call_id":"c2","content":"r2"}
				]
			}`,
			wantJSON: `{
				"user_input":"并行查",
				"tool_uses":[
					{"id":"c1","name":"f1","input":{}},
					{"id":"c2","name":"f2","input":{"x":1}}
				],
				"tool_results":[
					{"tool_use_id":"c1","content":"r1"},
					{"tool_use_id":"c2","content":"r2"}
				]
			}`,
		},
		{
			name: "multi turn keeps only last group",
			body: `{
				"messages": [
					{"role":"user","content":"原始任务"},
					{"role":"assistant","content":null,"tool_calls":[
						{"id":"c1","type":"function","function":{"name":"fa","arguments":"{}"}}
					]},
					{"role":"tool","tool_call_id":"c1","content":"OK1"},
					{"role":"assistant","content":null,"tool_calls":[
						{"id":"c2","type":"function","function":{"name":"fb","arguments":"{\"k\":2}"}}
					]},
					{"role":"tool","tool_call_id":"c2","content":"OK2"}
				]
			}`,
			wantJSON: `{
				"user_input":"原始任务",
				"tool_uses":[{"id":"c2","name":"fb","input":{"k":2}}],
				"tool_results":[{"tool_use_id":"c2","content":"OK2"}]
			}`,
		},
		{
			name: "content as text array",
			body: `{
				"messages": [
					{"role":"user","content":[{"type":"text","text":"数组形态"}]}
				]
			}`,
			wantJSON: `{"user_input":"数组形态"}`,
		},
		{name: "no messages", body: `{}`, wantJSON: ""},
		{name: "non-JSON", body: `xxx`, wantJSON: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SimplifyChatCompletionsRequest([]byte(tc.body))
			if tc.wantJSON == "" {
				assert.Empty(t, got)
				return
			}
			require.NotEmpty(t, got)
			assert.JSONEq(t, tc.wantJSON, got)
		})
	}
}

func TestSimplifyResponsesRequest(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantJSON string
	}{
		{
			name:     "input as string",
			body:     `{"model":"gpt-4o","input":"你好"}`,
			wantJSON: `{"model":"gpt-4o","user_input":"你好"}`,
		},
		{
			name: "first turn message array",
			body: `{
				"input": [
					{"type":"message","role":"user","content":[{"type":"input_text","text":"首轮"}]}
				]
			}`,
			wantJSON: `{"user_input":"首轮"}`,
		},
		{
			name: "single function call round",
			body: `{
				"input": [
					{"type":"message","role":"user","content":[{"type":"input_text","text":"查信息"}]},
					{"type":"function_call","call_id":"fc_1","name":"lookup","arguments":"{\"id\":7}"},
					{"type":"function_call_output","call_id":"fc_1","output":"result"}
				]
			}`,
			wantJSON: `{
				"user_input":"查信息",
				"tool_uses":[{"id":"fc_1","name":"lookup","input":{"id":7}}],
				"tool_results":[{"tool_use_id":"fc_1","content":"result"}]
			}`,
		},
		{
			name: "parallel function calls",
			body: `{
				"input": [
					{"type":"message","role":"user","content":[{"type":"input_text","text":"并行"}]},
					{"type":"function_call","call_id":"f1","name":"a","arguments":"{}"},
					{"type":"function_call","call_id":"f2","name":"b","arguments":"{}"},
					{"type":"function_call_output","call_id":"f1","output":"out1"},
					{"type":"function_call_output","call_id":"f2","output":"out2"}
				]
			}`,
			wantJSON: `{
				"user_input":"并行",
				"tool_uses":[
					{"id":"f1","name":"a","input":{}},
					{"id":"f2","name":"b","input":{}}
				],
				"tool_results":[
					{"tool_use_id":"f1","content":"out1"},
					{"tool_use_id":"f2","content":"out2"}
				]
			}`,
		},
		{
			name: "multi turn keeps only last group",
			body: `{
				"input": [
					{"type":"message","role":"user","content":[{"type":"input_text","text":"原始任务"}]},
					{"type":"function_call","call_id":"fa","name":"x","arguments":"{}"},
					{"type":"function_call_output","call_id":"fa","output":"ra"},
					{"type":"function_call","call_id":"fb","name":"y","arguments":"{\"k\":2}"},
					{"type":"function_call_output","call_id":"fb","output":"rb"}
				]
			}`,
			wantJSON: `{
				"user_input":"原始任务",
				"tool_uses":[{"id":"fb","name":"y","input":{"k":2}}],
				"tool_results":[{"tool_use_id":"fb","content":"rb"}]
			}`,
		},
		{name: "no input field", body: `{"model":"x"}`, wantJSON: ""},
		{name: "non-JSON", body: `xxx`, wantJSON: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SimplifyResponsesRequest([]byte(tc.body))
			if tc.wantJSON == "" {
				assert.Empty(t, got)
				return
			}
			require.NotEmpty(t, got)
			assert.JSONEq(t, tc.wantJSON, got)
		})
	}
}

func TestSimplifyRequestBody_Dispatch(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantJSON  string // 走 simplify 路径时校验 JSON
		wantExact string // 走 fallthrough 原样返回时校验 string
	}{
		{
			name: "anthropic dispatched",
			body: `{
				"messages":[
					{"role":"user","content":"hi"},
					{"role":"assistant","content":[
						{"type":"tool_use","id":"tu_1","name":"f","input":{}}
					]},
					{"role":"user","content":[
						{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}
					]}
				]
			}`,
			wantJSON: `{
				"user_input":"hi",
				"tool_uses":[{"id":"tu_1","name":"f","input":{}}],
				"tool_results":[{"tool_use_id":"tu_1","content":"ok"}]
			}`,
		},
		{
			name: "chatcompletions dispatched",
			body: `{
				"messages":[
					{"role":"user","content":"hi"},
					{"role":"assistant","content":null,"tool_calls":[
						{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}
					]},
					{"role":"tool","tool_call_id":"c1","content":"ok"}
				]
			}`,
			wantJSON: `{
				"user_input":"hi",
				"tool_uses":[{"id":"c1","name":"f","input":{}}],
				"tool_results":[{"tool_use_id":"c1","content":"ok"}]
			}`,
		},
		{
			name:     "responses dispatched",
			body:     `{"input":"hello"}`,
			wantJSON: `{"user_input":"hello"}`,
		},
		{
			name:      "fallthrough returns original body",
			body:      `{"foo":"bar"}`,
			wantExact: `{"foo":"bar"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SimplifyRequestBody([]byte(tc.body))
			if tc.wantJSON != "" {
				assert.JSONEq(t, tc.wantJSON, got)
				return
			}
			assert.Equal(t, tc.wantExact, got)
		})
	}
}

func TestExtractTextFromContent_Edge(t *testing.T) {
	// content 是 image-only 数组（无 text 块）→ 返回空串，使 user_input 回溯
	body := `{
		"messages": [
			{"role":"user","content":"原始问题"},
			{"role":"assistant","content":[
				{"type":"tool_use","id":"tu_1","name":"f","input":{}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"image","source":{"type":"base64","data":"AAAA"}}]}
			]}
		]
	}`
	got := SimplifyAnthropicRequest([]byte(body))
	require.NotEmpty(t, got)
	assert.JSONEq(t, `{
		"user_input":"原始问题",
		"tool_uses":[{"id":"tu_1","name":"f","input":{}}],
		"tool_results":[{"tool_use_id":"tu_1","content":[{"type":"image","source":{"type":"base64","data":"AAAA"}}]}]
	}`, got)
}
