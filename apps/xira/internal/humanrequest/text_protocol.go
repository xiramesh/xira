package humanrequest

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

const textReferencePrefix = "HR-"

type TextResponseCommand struct {
	CorrelationToken string
	Answer           string
}

// TextReference renders the full 128-bit correlation UUID as one copyable,
// case-insensitive protocol reference. Short prefixes are never accepted.
// coverage: contract (100% required)
func TextReference(correlation string) (string, error) {
	id, err := uuid.Parse(strings.TrimSpace(correlation))
	if err != nil {
		return "", fmt.Errorf("%w: human request correlation must be a UUID", ErrValidation)
	}
	return textReferencePrefix + strings.ToUpper(strings.ReplaceAll(id.String(), "-", "")), nil
}

// ParseTextResponse distinguishes ordinary chat from explicit protocol
// traffic. recognized=true is sticky for every /answer command, including
// malformed commands, so callers never feed protocol secrets into Agent Turn.
// coverage: contract (100% required)
func ParseTextResponse(content string) (TextResponseCommand, bool, error) {
	trimmed := strings.TrimSpace(content)
	command, rest := splitFirstField(trimmed)
	if !strings.EqualFold(command, "/answer") {
		return TextResponseCommand{}, false, nil
	}
	reference, answer := splitFirstField(rest)
	if reference == "" {
		return TextResponseCommand{}, true, fmt.Errorf("%w: /answer requires a request reference", ErrValidation)
	}
	correlation, err := correlationFromTextReference(reference)
	if err != nil {
		return TextResponseCommand{}, true, err
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return TextResponseCommand{}, true, fmt.Errorf("%w: /answer requires an answer", ErrValidation)
	}
	return TextResponseCommand{CorrelationToken: correlation, Answer: answer}, true, nil
}

// RenderTextRequest presents a HumanRequest without platform-specific markup.
// coverage: contract (100% required)
func RenderTextRequest(req HumanRequest) (string, error) {
	reference, err := TextReference(req.CorrelationToken)
	if err != nil {
		return "", err
	}
	question := strings.TrimSpace(req.Question)
	if question == "" {
		return "", fmt.Errorf("%w: human request question is required", ErrValidation)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "[请求 %s]\n%s\n\n", reference, question)
	if len(req.Options) == 0 {
		fmt.Fprintf(&out, "回复：\n/answer %s <回答>", reference)
		return out.String(), nil
	}
	out.WriteString("可选回答：\n")
	for _, option := range req.Options {
		value := strings.TrimSpace(option.ID)
		if value == "" {
			value = strings.TrimSpace(option.Label)
		}
		if value == "" {
			continue
		}
		label := strings.TrimSpace(option.Label)
		if label != "" && !strings.EqualFold(label, value) {
			fmt.Fprintf(&out, "- %s：%s\n", value, label)
		}
		fmt.Fprintf(&out, "/answer %s %s\n", reference, value)
	}
	return strings.TrimSpace(out.String()), nil
}

// NormalizeTextAnswer binds a user answer to declared options when present;
// freeform requests preserve the trimmed answer.
// coverage: contract (100% required)
func NormalizeTextAnswer(req HumanRequest, answer string) (string, error) {
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "", fmt.Errorf("%w: answer is required", ErrValidation)
	}
	if len(req.Options) == 0 {
		return answer, nil
	}
	for _, option := range req.Options {
		id := strings.TrimSpace(option.ID)
		label := strings.TrimSpace(option.Label)
		if id != "" && strings.EqualFold(answer, id) {
			return id, nil
		}
		if label != "" && strings.EqualFold(answer, label) {
			if id != "" {
				return id, nil
			}
			return label, nil
		}
	}
	return "", fmt.Errorf("%w: answer does not match a declared option", ErrValidation)
}

func correlationFromTextReference(reference string) (string, error) {
	reference = strings.TrimSpace(reference)
	if len(reference) != len(textReferencePrefix)+32 || !strings.EqualFold(reference[:len(textReferencePrefix)], textReferencePrefix) {
		return "", fmt.Errorf("%w: invalid human request reference", ErrValidation)
	}
	hex := reference[len(textReferencePrefix):]
	id, err := uuid.Parse(hex)
	if err != nil {
		return "", fmt.Errorf("%w: invalid human request reference", ErrValidation)
	}
	return id.String(), nil
}

func splitFirstField(value string) (string, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	index := strings.IndexFunc(value, unicode.IsSpace)
	if index < 0 {
		return value, ""
	}
	return value[:index], strings.TrimSpace(value[index:])
}
