package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zereker/llm-gateway/internal/domain"
)

var (
	ErrUnsupportedDocument = errors.New("policy: unsupported document")
	ErrInvalidMutation     = errors.New("policy: invalid mutation")
)

const documentTextKey = "text"

type DocumentAdapter interface {
	Extract(body []byte, protocol domain.Protocol, modality domain.Modality) ([]TextSegment, error)
	Apply(body []byte, protocol domain.Protocol, modality domain.Modality, mutations []Mutation) ([]byte, error)
}

type JSONDocumentAdapter struct{}

func (JSONDocumentAdapter) Extract(body []byte, _ domain.Protocol, modality domain.Modality) ([]TextSegment, error) {
	doc, err := decodeDocument(body)
	if err != nil {
		return nil, err
	}

	root, ok := doc.(map[string]any)
	if !ok {
		return nil, ErrUnsupportedDocument
	}

	var out []TextSegment

	addString := func(target string, value any) {
		if text, ok := value.(string); ok && text != "" {
			out = append(out, TextSegment{Target: target, Text: []byte(text)})
		}
	}

	for _, key := range []string{"system", "instructions", "prompt"} {
		if value, exists := root[key]; exists {
			collectText(value, "/"+escapePointer(key), &out, true)
		}
	}

	if value, exists := root["systemInstruction"]; exists {
		collectGeminiContents([]any{value}, "/systemInstruction", &out, false)
	}

	if value, exists := root["messages"]; exists {
		collectMessages(value, "/messages", &out)
	}

	if value, exists := root["input"]; exists {
		if modality == domain.ModalityEmbedding {
			collectText(value, "/input", &out, true)
		} else {
			collectInput(value, "/input", &out)
		}
	}

	if value, exists := root[documentTextKey]; exists {
		addString("/text", value)
	}

	if value, exists := root["content"]; exists {
		collectText(value, "/content", &out, true)
	}

	if value, exists := root["choices"]; exists {
		collectChoices(value, "/choices", &out)
	}

	if value, exists := root["contents"]; exists {
		collectGeminiContents(value, "/contents", &out, true)
	}

	if value, exists := root["candidates"]; exists {
		collectGeminiCandidates(value, "/candidates", &out)
	}

	if value, exists := root["output"]; exists {
		collectInput(value, "/output", &out)
	}

	return out, nil
}

func collectChoices(value any, path string, out *[]TextSegment) {
	items, ok := value.([]any)
	if !ok {
		return
	}

	for i, item := range items {
		choice, ok := item.(map[string]any)
		if !ok {
			continue
		}

		if text, exists := choice[documentTextKey]; exists {
			collectText(text, path+"/"+strconv.Itoa(i)+"/text", out, false)
		}

		for _, container := range []string{"message", "delta"} {
			object, ok := choice[container].(map[string]any)
			if !ok {
				continue
			}

			if content, exists := object["content"]; exists {
				collectText(content, path+"/"+strconv.Itoa(i)+"/"+container+"/content", out, true)
			}
		}
	}
}

func collectGeminiCandidates(value any, path string, out *[]TextSegment) {
	items, ok := value.([]any)
	if !ok {
		return
	}

	for i, item := range items {
		candidate, ok := item.(map[string]any)
		if !ok {
			continue
		}

		if content, exists := candidate["content"]; exists {
			collectGeminiContents([]any{content}, path+"/"+strconv.Itoa(i)+"/content", out, false)
		}
	}
}

func collectGeminiContents(value any, path string, out *[]TextSegment, indexed bool) {
	items, ok := value.([]any)
	if !ok {
		return
	}

	for i, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}

		itemPath := path
		if indexed {
			itemPath += "/" + strconv.Itoa(i)
		}

		parts, ok := object["parts"].([]any)
		if !ok {
			continue
		}

		for j, part := range parts {
			partObject, ok := part.(map[string]any)
			if !ok {
				continue
			}

			if text, exists := partObject[documentTextKey]; exists {
				collectText(text, itemPath+"/parts/"+strconv.Itoa(j)+"/text", out, false)
			}
		}
	}
}

func (a JSONDocumentAdapter) Apply(body []byte, protocol domain.Protocol, modality domain.Modality, mutations []Mutation) ([]byte, error) {
	segments, err := a.Extract(body, protocol, modality)
	if err != nil {
		return nil, err
	}

	allowed := make(map[string]struct{}, len(segments))
	for _, segment := range segments {
		allowed[segment.Target] = struct{}{}
	}

	doc, err := decodeDocument(body)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(mutations))
	for _, mutation := range mutations {
		if mutation.Kind != MutationRedact || !utf8.Valid(mutation.Replacement) {
			return nil, fmt.Errorf("%w: target %q has invalid replacement", ErrInvalidMutation, mutation.Target)
		}

		if _, ok := allowed[mutation.Target]; !ok {
			return nil, fmt.Errorf("%w: target %q is not an extracted text node", ErrInvalidMutation, mutation.Target)
		}

		if _, duplicate := seen[mutation.Target]; duplicate {
			return nil, fmt.Errorf("%w: duplicate target %q", ErrInvalidMutation, mutation.Target)
		}

		seen[mutation.Target] = struct{}{}

		if err := setPointer(doc, mutation.Target, string(mutation.Replacement)); err != nil {
			return nil, err
		}
	}

	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("policy: rebuild document: %w", err)
	}

	return encoded, nil
}

func decodeDocument(body []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	var doc any
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupportedDocument, err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrUnsupportedDocument
	}

	return doc, nil
}

func collectMessages(value any, path string, out *[]TextSegment) {
	items, ok := value.([]any)
	if !ok {
		return
	}

	for i, item := range items {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}

		if content, exists := message["content"]; exists {
			collectText(content, path+"/"+strconv.Itoa(i)+"/content", out, true)
		}
	}
}

func collectInput(value any, path string, out *[]TextSegment) {
	switch typed := value.(type) {
	case string:
		if typed != "" {
			*out = append(*out, TextSegment{Target: path, Text: []byte(typed)})
		}
	case []any:
		for i, item := range typed {
			itemPath := path + "/" + strconv.Itoa(i)
			if object, ok := item.(map[string]any); ok {
				if content, exists := object["content"]; exists {
					collectText(content, itemPath+"/content", out, true)
				}

				if text, exists := object[documentTextKey]; exists {
					collectText(text, itemPath+"/text", out, false)
				}

				continue
			}

			collectText(item, itemPath, out, false)
		}
	}
}

func collectText(value any, path string, out *[]TextSegment, nested bool) {
	switch typed := value.(type) {
	case string:
		if typed != "" {
			*out = append(*out, TextSegment{Target: path, Text: []byte(typed)})
		}
	case []any:
		for i, item := range typed {
			itemPath := path + "/" + strconv.Itoa(i)

			object, ok := item.(map[string]any)
			if !ok {
				if nested {
					collectText(item, itemPath, out, false)
				}

				continue
			}

			for _, key := range []string{documentTextKey, "content"} {
				if child, exists := object[key]; exists {
					collectText(child, itemPath+"/"+key, out, true)
				}
			}
		}
	}
}

func setPointer(doc any, pointer string, replacement string) error {
	parts, err := parsePointer(pointer)
	if err != nil || len(parts) == 0 {
		return fmt.Errorf("%w: malformed target %q", ErrInvalidMutation, pointer)
	}

	current := doc
	for _, part := range parts[:len(parts)-1] {
		switch typed := current.(type) {
		case map[string]any:
			next, exists := typed[part]
			if !exists {
				return fmt.Errorf("%w: target %q not found", ErrInvalidMutation, pointer)
			}

			current = next
		case []any:
			index, indexErr := strconv.Atoi(part)
			if indexErr != nil || index < 0 || index >= len(typed) {
				return fmt.Errorf("%w: target %q not found", ErrInvalidMutation, pointer)
			}

			current = typed[index]
		default:
			return fmt.Errorf("%w: target %q not found", ErrInvalidMutation, pointer)
		}
	}

	leaf := parts[len(parts)-1]
	switch typed := current.(type) {
	case map[string]any:
		if _, ok := typed[leaf].(string); !ok {
			return fmt.Errorf("%w: target %q is not text", ErrInvalidMutation, pointer)
		}

		typed[leaf] = replacement
	case []any:
		index, indexErr := strconv.Atoi(leaf)
		if indexErr != nil || index < 0 || index >= len(typed) {
			return fmt.Errorf("%w: target %q not found", ErrInvalidMutation, pointer)
		}

		if _, ok := typed[index].(string); !ok {
			return fmt.Errorf("%w: target %q is not text", ErrInvalidMutation, pointer)
		}

		typed[index] = replacement
	default:
		return fmt.Errorf("%w: target %q not found", ErrInvalidMutation, pointer)
	}

	return nil
}

func parsePointer(pointer string) ([]string, error) {
	if pointer == "" || pointer[0] != '/' {
		return nil, ErrInvalidMutation
	}

	raw := strings.Split(pointer[1:], "/")

	parts := make([]string, len(raw))
	for i, part := range raw {
		part = strings.ReplaceAll(part, "~1", "/")
		part = strings.ReplaceAll(part, "~0", "~")
		parts[i] = part
	}

	return parts, nil
}

func escapePointer(value string) string {
	value = strings.ReplaceAll(value, "~", "~0")

	return strings.ReplaceAll(value, "/", "~1")
}
