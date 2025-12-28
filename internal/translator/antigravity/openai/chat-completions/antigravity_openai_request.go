// Package openai provides request translation functionality for OpenAI to Gemini CLI API compatibility.
// It converts OpenAI Chat Completions requests into Gemini CLI compatible JSON using gjson/sjson only.
package chat_completions

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator/gemini/common"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// cleanToolSchema removes unsupported JSON Schema fields that Gemini doesn't accept
var additionalPropsRegex = regexp.MustCompile(`"additionalProperties"\s*:\s*(true|false)\s*,?`)

func cleanToolSchema(schema string) string {
	// Remove all "additionalProperties": true/false patterns
	cleaned := additionalPropsRegex.ReplaceAllString(schema, "")
	// Clean up any trailing commas before closing braces
	cleaned = strings.ReplaceAll(cleaned, ",}", "}")
	cleaned = strings.ReplaceAll(cleaned, ", }", "}")
	return cleaned
}

const geminiCLIFunctionThoughtSignature = "skip_thought_signature_validator"

// ConvertOpenAIRequestToAntigravity converts an OpenAI Chat Completions request (raw JSON)
// into a complete Gemini CLI request JSON. All JSON construction uses sjson and lookups use gjson.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Gemini CLI API format
func ConvertOpenAIRequestToAntigravity(modelName string, inputRawJSON []byte, _ bool) []byte {
	log.Debugf("Input OpenAI Request: %s", string(inputRawJSON))
	rawJSON := bytes.Clone(inputRawJSON)

	// Base envelope (no default thinkingConfig)
	out := []byte(`{"project":"","request":{"contents":[]},"model":"gemini-2.5-pro"}`)

	// Model
	out, _ = sjson.SetBytes(out, "model", modelName)
	includeToolIDs := strings.Contains(strings.ToLower(modelName), "claude")
	genToolCallID := func() string {
		const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		var b strings.Builder
		for i := 0; i < 24; i++ {
			n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(letters))))
			b.WriteByte(letters[n.Int64()])
		}
		return "toolu_" + b.String()
	}

	// Reasoning effort -> thinkingBudget/include_thoughts
	// Note: OpenAI official fields take precedence over extra_body.google.thinking_config
	re := gjson.GetBytes(rawJSON, "reasoning_effort")
	hasOfficialThinking := re.Exists()
	if hasOfficialThinking && util.ModelSupportsThinking(modelName) {
		effort := strings.ToLower(strings.TrimSpace(re.String()))
		if util.IsGemini3Model(modelName) {
			switch effort {
			case "none":
				out, _ = sjson.DeleteBytes(out, "request.generationConfig.thinkingConfig")
			case "auto":
				includeThoughts := true
				out = util.ApplyGeminiCLIThinkingLevel(out, "", &includeThoughts)
			default:
				if level, ok := util.ValidateGemini3ThinkingLevel(modelName, effort); ok {
					out = util.ApplyGeminiCLIThinkingLevel(out, level, nil)
				}
			}
		} else if !util.ModelUsesThinkingLevels(modelName) {
			out = util.ApplyReasoningEffortToGeminiCLI(out, effort)
		}
	}

	// Cherry Studio extension extra_body.google.thinking_config (effective only when official fields are absent)
	// Only apply for models that use numeric budgets, not discrete levels.
	if !hasOfficialThinking && util.ModelSupportsThinking(modelName) && !util.ModelUsesThinkingLevels(modelName) {
		if tc := gjson.GetBytes(rawJSON, "extra_body.google.thinking_config"); tc.Exists() && tc.IsObject() {
			var setBudget bool
			var budget int

			if v := tc.Get("thinkingBudget"); v.Exists() {
				budget = int(v.Int())
				out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget", budget)
				setBudget = true
			} else if v := tc.Get("thinking_budget"); v.Exists() {
				budget = int(v.Int())
				out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget", budget)
				setBudget = true
			}

			if v := tc.Get("includeThoughts"); v.Exists() {
				out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.include_thoughts", v.Bool())
			} else if v := tc.Get("include_thoughts"); v.Exists() {
				out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.include_thoughts", v.Bool())
			} else if setBudget && budget != 0 {
				out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.include_thoughts", true)
			}
		}
	}

	// Claude/Anthropic API format: thinking.type == "enabled" with budget_tokens
	// This allows Claude Code and other Claude API clients to pass thinking configuration
	if !gjson.GetBytes(out, "request.generationConfig.thinkingConfig").Exists() && util.ModelSupportsThinking(modelName) {
		if t := gjson.GetBytes(rawJSON, "thinking"); t.Exists() && t.IsObject() {
			if t.Get("type").String() == "enabled" {
				if b := t.Get("budget_tokens"); b.Exists() && b.Type == gjson.Number {
					budget := int(b.Int())
					out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.thinkingBudget", budget)
					out, _ = sjson.SetBytes(out, "request.generationConfig.thinkingConfig.include_thoughts", true)
				}
			}
		}
	}

	// Temperature/top_p/top_k/max_tokens
	if tr := gjson.GetBytes(rawJSON, "temperature"); tr.Exists() && tr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.temperature", tr.Num)
	}
	if tpr := gjson.GetBytes(rawJSON, "top_p"); tpr.Exists() && tpr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.topP", tpr.Num)
	}
	if tkr := gjson.GetBytes(rawJSON, "top_k"); tkr.Exists() && tkr.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.topK", tkr.Num)
	}
	if maxTok := gjson.GetBytes(rawJSON, "max_tokens"); maxTok.Exists() && maxTok.Type == gjson.Number {
		out, _ = sjson.SetBytes(out, "request.generationConfig.maxOutputTokens", maxTok.Num)
	}

	// Map OpenAI modalities -> Gemini CLI request.generationConfig.responseModalities
	// e.g. "modalities": ["image", "text"] -> ["IMAGE", "TEXT"]
	if mods := gjson.GetBytes(rawJSON, "modalities"); mods.Exists() && mods.IsArray() {
		var responseMods []string
		for _, m := range mods.Array() {
			switch strings.ToLower(m.String()) {
			case "text":
				responseMods = append(responseMods, "TEXT")
			case "image":
				responseMods = append(responseMods, "IMAGE")
			}
		}
		if len(responseMods) > 0 {
			out, _ = sjson.SetBytes(out, "request.generationConfig.responseModalities", responseMods)
		}
	}

	// OpenRouter-style image_config support
	// If the input uses top-level image_config.aspect_ratio, map it into request.generationConfig.imageConfig.aspectRatio.
	if imgCfg := gjson.GetBytes(rawJSON, "image_config"); imgCfg.Exists() && imgCfg.IsObject() {
		if ar := imgCfg.Get("aspect_ratio"); ar.Exists() && ar.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "request.generationConfig.imageConfig.aspectRatio", ar.Str)
		}
		if size := imgCfg.Get("image_size"); size.Exists() && size.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "request.generationConfig.imageConfig.imageSize", size.Str)
		}
	}

	// messages -> systemInstruction + contents
	messages := gjson.GetBytes(rawJSON, "messages")
	if messages.IsArray() {
		arr := messages.Array()
		// First pass: assistant tool_calls id->name map
		tcID2Name := map[string]string{}
		for i := 0; i < len(arr); i++ {
			m := arr[i]
			if m.Get("role").String() == "assistant" {
				tcs := m.Get("tool_calls")
				if tcs.IsArray() {
					for _, tc := range tcs.Array() {
						// Support both OpenAI format (type:"function") and direct format
						tcType := tc.Get("type").String()
						if tcType == "function" || tcType == "" {
							id := tc.Get("id").String()
							name := tc.Get("function.name").String()
							if id != "" && name != "" {
								tcID2Name[id] = name
							}
						}
					}
				}
				// Also check for Anthropic-style tool_use in content
				content := m.Get("content")
				if content.IsArray() {
					for _, item := range content.Array() {
						if item.Get("type").String() == "tool_use" {
							id := item.Get("id").String()
							name := item.Get("name").String()
							if id != "" && name != "" {
								tcID2Name[id] = name
							}
						}
					}
				}
			}
		}

		// Second pass build systemInstruction/tool responses cache
		toolResponses := map[string]string{} // tool_call_id -> response text
		for i := 0; i < len(arr); i++ {
			m := arr[i]
			role := m.Get("role").String()
			if role == "tool" {
				toolCallID := m.Get("tool_call_id").String()
				if toolCallID != "" {
					c := m.Get("content")
					toolResponses[toolCallID] = c.Raw
				}
			}
		}

		for i := 0; i < len(arr); i++ {
			m := arr[i]
			role := m.Get("role").String()
			content := m.Get("content")

			if role == "system" && len(arr) > 1 {
				// system -> request.systemInstruction as a user message style
				if content.Type == gjson.String {
					out, _ = sjson.SetBytes(out, "request.systemInstruction.role", "user")
					out, _ = sjson.SetBytes(out, "request.systemInstruction.parts.0.text", content.String())
				} else if content.IsObject() && content.Get("type").String() == "text" {
					out, _ = sjson.SetBytes(out, "request.systemInstruction.role", "user")
					out, _ = sjson.SetBytes(out, "request.systemInstruction.parts.0.text", content.Get("text").String())
				} else if content.IsArray() {
					contents := content.Array()
					if len(contents) > 0 {
						out, _ = sjson.SetBytes(out, "request.systemInstruction.role", "user")
						for j := 0; j < len(contents); j++ {
							out, _ = sjson.SetBytes(out, fmt.Sprintf("request.systemInstruction.parts.%d.text", j), contents[j].Get("text").String())
						}
					}
				}
			} else if role == "user" || (role == "system" && len(arr) == 1) {
				// Build single user content node to avoid splitting into multiple contents
				node := []byte(`{"role":"user","parts":[]}`)
				if content.Type == gjson.String {
					node, _ = sjson.SetBytes(node, "parts.0.text", content.String())
				} else if content.IsArray() {
					items := content.Array()
					p := 0
					for _, item := range items {
						switch item.Get("type").String() {
						case "text":
							node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".text", item.Get("text").String())
							p++
						case "image_url":
							imageURL := item.Get("image_url.url").String()
							if len(imageURL) > 5 {
								pieces := strings.SplitN(imageURL[5:], ";", 2)
								if len(pieces) == 2 && len(pieces[1]) > 7 {
									mime := pieces[0]
									data := pieces[1][7:]
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mime_type", mime)
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", data)
									p++
								}
							}
						case "file":
							filename := item.Get("file.filename").String()
							fileData := item.Get("file.file_data").String()
							ext := ""
							if sp := strings.Split(filename, "."); len(sp) > 1 {
								ext = sp[len(sp)-1]
							}
							if mimeType, ok := misc.MimeTypes[ext]; ok {
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mime_type", mimeType)
								node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", fileData)
								p++
							} else {
								log.Warnf("Unknown file name extension '%s' in user message, skip", ext)
							}
						case "tool_result":
							// Handle Anthropic-style tool_result -> functionResponse
							toolID := item.Get("tool_use_id").String()
							var contentStr string

							// Extract content string from various formats
							c := item.Get("content")
							if c.IsArray() {
								// Anthropic sends array of text/image. We only support text result for now.
								for _, sub := range c.Array() {
									if sub.Get("type").String() == "text" {
										contentStr += sub.Get("text").String()
									}
								}
							} else {
								contentStr = c.String()
							}

							if name, ok := tcID2Name[toolID]; ok {
								// We need to switch role to 'function' for this part?
								// Gemini v1beta supports role: function. But we are inside a 'user' message loop.
								// If we mix text and functionResponse in 'user' message, it might fail?
								// Gemini docs say: "The role must be 'function' if the content is a FunctionResponse."
								// So we probably need to break this out into a separate message if possible,
								// OR change the role of THIS message to function if it ONLY contains tool results.
								// However, assuming mixed content isn't allowed, we try to add it as a part with role='function'
								// if we could start a new message. But here we are building `parts` for a single message.

								// Actually, earlier logs showed we send separate messages for user query?
								// Let's try adding it as a part. If mixed roles are problem, we might need more complex logic.
								// But for now, let's treat it as a functionResponse part.
								// Wait, functionResponse part is: { "functionResponse": { ... } }
								// It doesn't have a role INSIDE the part. The MESSAGE has a role.
								// If the message has role 'user', can it contain 'functionResponse'?
								// Gemini 1.5 Pro allows role 'user' for function response.

								toolNode := []byte(`{}`)
								toolNode, _ = sjson.SetBytes(toolNode, "functionResponse.name", name)
								if includeToolIDs && toolID != "" {
									toolNode, _ = sjson.SetBytes(toolNode, "functionResponse.id", toolID)
								}
								toolNode, _ = sjson.SetBytes(toolNode, "functionResponse.response.result", contentStr) // Use simple result

								// Add as a part
								node, _ = sjson.SetRawBytes(node, "parts."+itoa(p), toolNode)
								p++
							}
						}
					}
				}
				out, _ = sjson.SetRawBytes(out, "request.contents.-1", node)
			} else if role == "tool" {
				continue // handled in assistant block
			} else if role == "assistant" {
				node := []byte(`{"role":"model","parts":[]}`)
				p := 0
				if content.Type == gjson.String {
					node, _ = sjson.SetBytes(node, "parts.-1.text", content.String())
					p++
				} else if content.IsArray() {
					// Assistant multimodal content (e.g. text + image) -> single model content with parts
					for _, item := range content.Array() {
						switch item.Get("type").String() {
						case "text":
							node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".text", item.Get("text").String())
							p++
						case "image_url":
							// If the assistant returned an inline data URL, preserve it for history fidelity.
							imageURL := item.Get("image_url.url").String()
							if len(imageURL) > 5 { // expect data:...
								pieces := strings.SplitN(imageURL[5:], ";", 2)
								if len(pieces) == 2 && len(pieces[1]) > 7 {
									mime := pieces[0]
									data := pieces[1][7:]
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.mime_type", mime)
									node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".inlineData.data", data)
									p++
								}
							}
						case "tool_use":
							// Handle Anthropic-style tool_use -> functionCall
							fname := item.Get("name").String()
							fargs := item.Get("input").Raw
							fid := item.Get("id").String()
							if includeToolIDs && fid == "" {
								fid = genToolCallID()
							}

							fcNode := []byte(`{}`)
							fcNode, _ = sjson.SetBytes(fcNode, "functionCall.name", fname)
							if includeToolIDs && fid != "" {
								fcNode, _ = sjson.SetBytes(fcNode, "functionCall.id", fid)
							}

							// Ensure args is an object
							if gjson.Valid(fargs) {
								fcNode, _ = sjson.SetRawBytes(fcNode, "functionCall.args", []byte(fargs))
							} else {
								fcNode, _ = sjson.SetBytes(fcNode, "functionCall.args.params", []byte(fargs))
							}

							node, _ = sjson.SetRawBytes(node, "parts."+itoa(p), fcNode)
							p++
						}
					}
				}

				// Tool calls -> single model content with functionCall parts
				tcs := m.Get("tool_calls")
				if tcs.IsArray() {
					fIDs := make([]string, 0)
					for _, tc := range tcs.Array() {
						// Support both OpenAI format (type:"function") and direct format
						tcType := tc.Get("type").String()
						if tcType != "function" && tcType != "" {
							continue
						}
						fid := tc.Get("id").String()
						fname := tc.Get("function.name").String()
						fargs := tc.Get("function.arguments").String()
						if includeToolIDs && fid == "" {
							fid = genToolCallID()
						}
						node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".functionCall.name", fname)
						if includeToolIDs && fid != "" {
							node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".functionCall.id", fid)
							if fname != "" {
								tcID2Name[fid] = fname
							}
						}
						if gjson.Valid(fargs) {
							node, _ = sjson.SetRawBytes(node, "parts."+itoa(p)+".functionCall.args", []byte(fargs))
						} else {
							node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".functionCall.args.params", []byte(fargs))
						}
						// node, _ = sjson.SetBytes(node, "parts."+itoa(p)+".thoughtSignature", geminiCLIFunctionThoughtSignature) // Unsupported
						p++
						if fid != "" {
							fIDs = append(fIDs, fid)
						}
					}
					out, _ = sjson.SetRawBytes(out, "request.contents.-1", node)

					// Append a single tool content combining name + response per function
					toolNode := []byte(`{"role":"function","parts":[]}`)
					pp := 0
					for _, fid := range fIDs {
						if name, ok := tcID2Name[fid]; ok {
							// toolNode, _ = sjson.SetBytes(toolNode, "parts."+itoa(pp)+".functionResponse.id", fid) // Gemini doesn't use ID
							toolNode, _ = sjson.SetBytes(toolNode, "parts."+itoa(pp)+".functionResponse.name", name)
							if includeToolIDs {
								toolNode, _ = sjson.SetBytes(toolNode, "parts."+itoa(pp)+".functionResponse.id", fid)
							}
							resp := toolResponses[fid]
							if resp == "" {
								resp = "{}"
							}
							// Handle non-JSON output gracefully (matches dev branch approach)
							if resp != "null" {
								parsed := gjson.Parse(resp)
								if parsed.Type == gjson.JSON {
									toolNode, _ = sjson.SetRawBytes(toolNode, "parts."+itoa(pp)+".functionResponse.response.result", []byte(parsed.Raw))
								} else {
									toolNode, _ = sjson.SetBytes(toolNode, "parts."+itoa(pp)+".functionResponse.response.result", resp)
								}
							}
							pp++
						}
					}
					if pp > 0 {
						out, _ = sjson.SetRawBytes(out, "request.contents.-1", toolNode)
					}
				} else {
					out, _ = sjson.SetRawBytes(out, "request.contents.-1", node)
				}
			}
		}
	}

	// tools -> request.tools[0].functionDeclarations + request.tools[0].googleSearch passthrough
	tools := gjson.GetBytes(rawJSON, "tools")
	log.Debugf("[TOOLS DEBUG] tools exist: %v, isArray: %v, length: %d", tools.Exists(), tools.IsArray(), len(tools.Array()))
	if tools.IsArray() && len(tools.Array()) > 0 {
		toolNode := []byte(`{}`)
		hasTool := false
		hasFunction := false
		for i, t := range tools.Array() {
			toolType := t.Get("type").String()
			if i < 3 {
				log.Debugf("[TOOLS DEBUG] Tool %d type: '%s', has function: %v", i, toolType, t.Get("function").Exists())
				if i == 0 {
					// Log first 500 chars of first tool to see structure
					raw := t.Raw
					if len(raw) > 500 {
						raw = raw[:500]
					}
					log.Debugf("[TOOLS DEBUG] Tool 0 raw: %s", raw)
				}
			}
			if toolType == "function" {
				fn := t.Get("function")
				if fn.Exists() && fn.IsObject() {
					fnRaw := fn.Raw
					if fn.Get("parameters").Exists() {
						renamed, errRename := util.RenameKey(fnRaw, "parameters", "parametersJsonSchema")
						if errRename != nil {
							log.Warnf("Failed to rename parameters for tool '%s': %v", fn.Get("name").String(), errRename)
							var errSet error
							fnRaw, errSet = sjson.Set(fnRaw, "parametersJsonSchema.type", "object")
							if errSet != nil {
								log.Warnf("Failed to set default schema type for tool '%s': %v", fn.Get("name").String(), errSet)
								continue
							}
							fnRaw, errSet = sjson.SetRaw(fnRaw, "parametersJsonSchema.properties", `{}`)
							if errSet != nil {
								log.Warnf("Failed to set default schema properties for tool '%s': %v", fn.Get("name").String(), errSet)
								continue
							}
						} else {
							fnRaw = renamed
						}
					} else {
						var errSet error
						fnRaw, errSet = sjson.Set(fnRaw, "parametersJsonSchema.type", "object")
						if errSet != nil {
							log.Warnf("Failed to set default schema type for tool '%s': %v", fn.Get("name").String(), errSet)
							continue
						}
						fnRaw, errSet = sjson.SetRaw(fnRaw, "parametersJsonSchema.properties", `{}`)
						if errSet != nil {
							log.Warnf("Failed to set default schema properties for tool '%s': %v", fn.Get("name").String(), errSet)
							continue
						}
					}
					fnRaw, _ = sjson.Delete(fnRaw, "strict")
					fnRaw, _ = sjson.Delete(fnRaw, "cache_control")
					if !hasFunction {
						toolNode, _ = sjson.SetRawBytes(toolNode, "functionDeclarations", []byte("[]"))
					}
					tmp, errSet := sjson.SetRawBytes(toolNode, "functionDeclarations.-1", []byte(fnRaw))
					if errSet != nil {
						log.Warnf("Failed to append tool declaration for '%s': %v", fn.Get("name").String(), errSet)
						continue
					}
					toolNode = tmp
					hasFunction = true
					hasTool = true
				}
			} else if t.Get("name").Exists() {
				// Direct tool format (MCP tools from Cursor): {name, description, parameters/input_schema}
				// Convert to Gemini functionDeclarations format
				fnRaw := t.Raw

				// Handle input_schema (Anthropic/MCP format) -> parametersJsonSchema
				if t.Get("input_schema").Exists() {
					renamed, errRename := util.RenameKey(fnRaw, "input_schema", "parametersJsonSchema")
					if errRename != nil {
						log.Warnf("Failed to rename input_schema for tool '%s': %v", t.Get("name").String(), errRename)
						// Set default schema
						fnRaw, _ = sjson.Set(fnRaw, "parametersJsonSchema.type", "object")
						fnRaw, _ = sjson.SetRaw(fnRaw, "parametersJsonSchema.properties", `{}`)
						fnRaw, _ = sjson.Delete(fnRaw, "input_schema")
					} else {
						fnRaw = renamed
					}
				} else if t.Get("parameters").Exists() {
					// Handle parameters (OpenAI format)
					renamed, errRename := util.RenameKey(fnRaw, "parameters", "parametersJsonSchema")
					if errRename != nil {
						log.Warnf("Failed to rename parameters for direct tool '%s': %v", t.Get("name").String(), errRename)
						fnRaw, _ = sjson.Set(fnRaw, "parametersJsonSchema.type", "object")
						fnRaw, _ = sjson.SetRaw(fnRaw, "parametersJsonSchema.properties", `{}`)
					} else {
						fnRaw = renamed
					}
				} else {
					// No schema provided, set default
					fnRaw, _ = sjson.Set(fnRaw, "parametersJsonSchema.type", "object")
					fnRaw, _ = sjson.SetRaw(fnRaw, "parametersJsonSchema.properties", `{}`)
				}

				// Clean up unsupported fields
				fnRaw, _ = sjson.Delete(fnRaw, "strict")
				fnRaw, _ = sjson.Delete(fnRaw, "cache_control")
				fnRaw, _ = sjson.Delete(fnRaw, "input_schema")                              // Ensure removed if rename failed
				fnRaw, _ = sjson.Delete(fnRaw, "parametersJsonSchema.additionalProperties") // Not supported by Gemini

				if !hasFunction {
					toolNode, _ = sjson.SetRawBytes(toolNode, "functionDeclarations", []byte("[]"))
				}
				tmp, errSet := sjson.SetRawBytes(toolNode, "functionDeclarations.-1", []byte(fnRaw))
				if errSet != nil {
					log.Warnf("Failed to append direct tool declaration for '%s': %v", t.Get("name").String(), errSet)
					continue
				}
				toolNode = tmp
				hasFunction = true
				hasTool = true
			}
			if gs := t.Get("google_search"); gs.Exists() {
				var errSet error
				toolNode, errSet = sjson.SetRawBytes(toolNode, "googleSearch", []byte(gs.Raw))
				if errSet != nil {
					log.Warnf("Failed to set googleSearch tool: %v", errSet)
					continue
				}
				hasTool = true
			}
		}
		if hasTool {
			// Clean up nested additionalProperties that Gemini doesn't support
			cleanedToolNode := cleanToolSchema(string(toolNode))
			log.Debugf("[TOOLS DEBUG] Setting tools in request, toolNode length: %d", len(cleanedToolNode))
			out, _ = sjson.SetRawBytes(out, "request.tools", []byte("[]"))
			out, _ = sjson.SetRawBytes(out, "request.tools.0", []byte(cleanedToolNode))
		} else {
			log.Debugf("[TOOLS DEBUG] No tools to set (hasTool=false)")
		}
	}

	// tool_choice -> request.toolConfig.functionCallingConfig
	if tc := gjson.GetBytes(rawJSON, "tool_choice"); tc.Exists() {
		mode := ""
		switch {
		case tc.Type == gjson.String:
			switch strings.ToLower(strings.TrimSpace(tc.String())) {
			case "none":
				mode = "NONE"
			case "auto":
				mode = "AUTO"
			case "required":
				mode = "ANY"
			}
		case tc.IsObject():
			if tc.Get("type").String() == "function" {
				if name := tc.Get("function.name").String(); name != "" {
					mode = "ANY"
					out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.allowedFunctionNames", []string{name})
				}
			}
		}
		if mode != "" {
			out, _ = sjson.SetBytes(out, "request.toolConfig.functionCallingConfig.mode", mode)
		}
	}

	// Log the final request for debugging MCP tool issues
	log.Debugf("Final Antigravity Request: %s", string(out))

	return common.AttachDefaultSafetySettings(out, "request.safetySettings")
}

// itoa converts int to string without strconv import for few usages.
func itoa(i int) string { return fmt.Sprintf("%d", i) }
