package pricing

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	maxPricingExpressionBytes  = 4096
	maxPricingExpressionTokens = 2048
	maxPricingExpressionDepth  = 64
)

type expressionContext struct {
	UncachedInput   int64
	CacheReadInput  int64
	CacheWriteInput int64
	CacheWrite1h    int64
	CacheWrite12h   int64
	Output          int64
	AudioInput      int64
	AudioOutput     int64
	InputLength     int64
	ImageCount      int64
	Thinking        bool
	At              time.Time
}

type expressionResult struct {
	USDMicros int64
	Tier      string
}

type compiledExpression struct {
	source string
	root   expressionNode
}

func compileExpression(source string) (*compiledExpression, error) {
	source = strings.TrimSpace(source)
	if source == "" || len(source) > maxPricingExpressionBytes {
		return nil, errors.New("pricing expression is empty or too long")
	}
	lexer := expressionLexer{source: source}
	tokens, errTokens := lexer.scan()
	if errTokens != nil {
		return nil, errTokens
	}
	parser := expressionParser{tokens: tokens}
	root, errParse := parser.parseTernary()
	if errParse != nil {
		return nil, errParse
	}
	if parser.current().kind != expressionTokenEOF {
		return nil, fmt.Errorf("unexpected token %q", parser.current().text)
	}
	if root.valueType() != expressionValueNumber {
		return nil, errors.New("pricing expression must produce a number")
	}
	return &compiledExpression{source: source, root: root}, nil
}

func (e *compiledExpression) Evaluate(ctx expressionContext) (expressionResult, error) {
	if e == nil || e.root == nil {
		return expressionResult{}, errors.New("pricing expression is unavailable")
	}
	for name, value := range map[string]int64{
		"p": ctx.UncachedInput, "cr": ctx.CacheReadInput, "cc": ctx.CacheWriteInput,
		"cc1h": ctx.CacheWrite1h, "cc12h": ctx.CacheWrite12h, "c": ctx.Output,
		"ai": ctx.AudioInput, "ao": ctx.AudioOutput, "len": ctx.InputLength,
		"image_count": ctx.ImageCount,
	} {
		if value < 0 {
			return expressionResult{}, fmt.Errorf("pricing expression variable %s cannot be negative", name)
		}
	}
	state := expressionEvaluation{ctx: ctx}
	value, errEvaluate := e.root.evaluate(&state)
	if errEvaluate != nil {
		return expressionResult{}, errEvaluate
	}
	if value.number < 0 || math.IsNaN(value.number) || math.IsInf(value.number, 0) {
		return expressionResult{}, errors.New("pricing expression produced an invalid cost")
	}
	scaled := value.number * float64(usdPerMillionScale)
	if scaled > float64(math.MaxInt64)-0.5 {
		return expressionResult{}, errors.New("pricing expression cost is out of range")
	}
	return expressionResult{USDMicros: int64(math.Round(scaled)), Tier: state.tier}, nil
}

type expressionValueType uint8

const (
	expressionValueNumber expressionValueType = iota
	expressionValueBool
	expressionValueString
)

type expressionValue struct {
	kind    expressionValueType
	number  float64
	boolean bool
	text    string
}

type expressionEvaluation struct {
	ctx  expressionContext
	tier string
}

type expressionNode interface {
	valueType() expressionValueType
	evaluate(*expressionEvaluation) (expressionValue, error)
}

type expressionLiteralNode struct{ value expressionValue }

func (n expressionLiteralNode) valueType() expressionValueType { return n.value.kind }
func (n expressionLiteralNode) evaluate(*expressionEvaluation) (expressionValue, error) {
	return n.value, nil
}

type expressionVariableNode struct{ name string }

func (expressionVariableNode) valueType() expressionValueType { return expressionValueNumber }
func (n expressionVariableNode) evaluate(state *expressionEvaluation) (expressionValue, error) {
	ctx := state.ctx
	var value float64
	switch n.name {
	case "p":
		value = float64(ctx.UncachedInput) / float64(usdPerMillionScale)
	case "cr":
		value = float64(ctx.CacheReadInput) / float64(usdPerMillionScale)
	case "cc":
		value = float64(ctx.CacheWriteInput) / float64(usdPerMillionScale)
	case "cc1h":
		value = float64(ctx.CacheWrite1h) / float64(usdPerMillionScale)
	case "cc12h":
		value = float64(ctx.CacheWrite12h) / float64(usdPerMillionScale)
	case "c":
		value = float64(ctx.Output) / float64(usdPerMillionScale)
	case "ai":
		value = float64(ctx.AudioInput) / float64(usdPerMillionScale)
	case "ao":
		value = float64(ctx.AudioOutput) / float64(usdPerMillionScale)
	case "len":
		value = float64(ctx.InputLength)
	case "image_count":
		value = float64(ctx.ImageCount)
	default:
		return expressionValue{}, fmt.Errorf("unknown pricing variable %q", n.name)
	}
	return expressionValue{kind: expressionValueNumber, number: value}, nil
}

type expressionBinaryNode struct {
	op          string
	left, right expressionNode
	kind        expressionValueType
}

func (n expressionBinaryNode) valueType() expressionValueType { return n.kind }
func (n expressionBinaryNode) evaluate(state *expressionEvaluation) (expressionValue, error) {
	left, errLeft := n.left.evaluate(state)
	if errLeft != nil {
		return expressionValue{}, errLeft
	}
	if n.op == "&&" && !left.boolean {
		return expressionValue{kind: expressionValueBool, boolean: false}, nil
	}
	if n.op == "||" && left.boolean {
		return expressionValue{kind: expressionValueBool, boolean: true}, nil
	}
	right, errRight := n.right.evaluate(state)
	if errRight != nil {
		return expressionValue{}, errRight
	}
	switch n.op {
	case "+":
		return expressionValue{kind: expressionValueNumber, number: left.number + right.number}, nil
	case "*":
		return expressionValue{kind: expressionValueNumber, number: left.number * right.number}, nil
	case "<":
		return expressionValue{kind: expressionValueBool, boolean: left.number < right.number}, nil
	case "<=":
		return expressionValue{kind: expressionValueBool, boolean: left.number <= right.number}, nil
	case ">":
		return expressionValue{kind: expressionValueBool, boolean: left.number > right.number}, nil
	case ">=":
		return expressionValue{kind: expressionValueBool, boolean: left.number >= right.number}, nil
	case "==":
		if left.kind == expressionValueBool {
			return expressionValue{kind: expressionValueBool, boolean: left.boolean == right.boolean}, nil
		}
		if left.kind == expressionValueString {
			return expressionValue{kind: expressionValueBool, boolean: left.text == right.text}, nil
		}
		return expressionValue{kind: expressionValueBool, boolean: left.number == right.number}, nil
	case "&&":
		return expressionValue{kind: expressionValueBool, boolean: left.boolean && right.boolean}, nil
	case "||":
		return expressionValue{kind: expressionValueBool, boolean: left.boolean || right.boolean}, nil
	default:
		return expressionValue{}, fmt.Errorf("unsupported pricing operator %q", n.op)
	}
}

type expressionConditionalNode struct {
	condition expressionNode
	whenTrue  expressionNode
	whenFalse expressionNode
	kind      expressionValueType
}

func (n expressionConditionalNode) valueType() expressionValueType { return n.kind }
func (n expressionConditionalNode) evaluate(state *expressionEvaluation) (expressionValue, error) {
	condition, errCondition := n.condition.evaluate(state)
	if errCondition != nil {
		return expressionValue{}, errCondition
	}
	if condition.boolean {
		return n.whenTrue.evaluate(state)
	}
	return n.whenFalse.evaluate(state)
}

type expressionCallNode struct {
	name string
	args []expressionNode
	kind expressionValueType
}

func (n expressionCallNode) valueType() expressionValueType { return n.kind }
func (n expressionCallNode) evaluate(state *expressionEvaluation) (expressionValue, error) {
	switch n.name {
	case "tier":
		label, _ := n.args[0].evaluate(state)
		value, errValue := n.args[1].evaluate(state)
		if errValue != nil {
			return expressionValue{}, errValue
		}
		state.tier = label.text
		return value, nil
	case "fixed":
		return n.args[0].evaluate(state)
	case "param":
		return expressionValue{kind: expressionValueBool, boolean: state.ctx.Thinking}, nil
	case "weekday":
		at := state.ctx.At.UTC()
		return expressionValue{kind: expressionValueNumber, number: float64(at.Weekday())}, nil
	case "hour":
		at := state.ctx.At.UTC()
		return expressionValue{kind: expressionValueNumber, number: float64(at.Hour())}, nil
	default:
		return expressionValue{}, fmt.Errorf("unsupported pricing function %q", n.name)
	}
}

type expressionTokenKind uint8

const (
	expressionTokenEOF expressionTokenKind = iota
	expressionTokenNumber
	expressionTokenIdentifier
	expressionTokenString
	expressionTokenSymbol
)

type expressionToken struct {
	kind expressionTokenKind
	text string
}

type expressionLexer struct {
	source string
	pos    int
}

func (l *expressionLexer) scan() ([]expressionToken, error) {
	tokens := make([]expressionToken, 0, 64)
	for l.pos < len(l.source) {
		char := l.source[l.pos]
		if unicode.IsSpace(rune(char)) {
			l.pos++
			continue
		}
		if len(tokens) >= maxPricingExpressionTokens {
			return nil, errors.New("pricing expression has too many tokens")
		}
		switch {
		case char >= '0' && char <= '9' || char == '.':
			start := l.pos
			dots := 0
			for l.pos < len(l.source) {
				current := l.source[l.pos]
				if current == '.' {
					dots++
					if dots > 1 {
						return nil, errors.New("invalid pricing number")
					}
					l.pos++
					continue
				}
				if current < '0' || current > '9' {
					break
				}
				l.pos++
			}
			text := l.source[start:l.pos]
			if text == "." {
				return nil, errors.New("invalid pricing number")
			}
			tokens = append(tokens, expressionToken{kind: expressionTokenNumber, text: text})
		case isExpressionIdentifierStart(char):
			start := l.pos
			l.pos++
			for l.pos < len(l.source) && isExpressionIdentifierPart(l.source[l.pos]) {
				l.pos++
			}
			tokens = append(tokens, expressionToken{kind: expressionTokenIdentifier, text: l.source[start:l.pos]})
		case char == '"':
			value, errString := l.scanString()
			if errString != nil {
				return nil, errString
			}
			tokens = append(tokens, expressionToken{kind: expressionTokenString, text: value})
		default:
			matched := ""
			for _, operator := range []string{"&&", "||", "<=", ">=", "=="} {
				if strings.HasPrefix(l.source[l.pos:], operator) {
					matched = operator
					break
				}
			}
			if matched == "" && strings.ContainsRune("?:+*<>() ,", rune(char)) {
				matched = string(char)
			}
			if matched == "" || matched == " " {
				return nil, fmt.Errorf("unsupported pricing character %q", char)
			}
			l.pos += len(matched)
			tokens = append(tokens, expressionToken{kind: expressionTokenSymbol, text: matched})
		}
	}
	tokens = append(tokens, expressionToken{kind: expressionTokenEOF})
	return tokens, nil
}

func (l *expressionLexer) scanString() (string, error) {
	l.pos++
	var value strings.Builder
	for l.pos < len(l.source) {
		char := l.source[l.pos]
		l.pos++
		if char == '"' {
			return value.String(), nil
		}
		if char == '\\' {
			if l.pos >= len(l.source) {
				return "", errors.New("unterminated pricing string")
			}
			escaped := l.source[l.pos]
			l.pos++
			if escaped != '"' && escaped != '\\' {
				return "", errors.New("unsupported pricing string escape")
			}
			char = escaped
		}
		value.WriteByte(char)
	}
	return "", errors.New("unterminated pricing string")
}

func isExpressionIdentifierStart(char byte) bool {
	return char == '_' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z'
}

func isExpressionIdentifierPart(char byte) bool {
	return isExpressionIdentifierStart(char) || char >= '0' && char <= '9'
}

type expressionParser struct {
	tokens []expressionToken
	pos    int
	depth  int
}

func (p *expressionParser) current() expressionToken { return p.tokens[p.pos] }
func (p *expressionParser) advance() expressionToken {
	token := p.current()
	if p.pos < len(p.tokens)-1 {
		p.pos++
	}
	return token
}
func (p *expressionParser) match(symbol string) bool {
	if p.current().kind == expressionTokenSymbol && p.current().text == symbol {
		p.advance()
		return true
	}
	return false
}
func (p *expressionParser) expect(symbol string) error {
	if !p.match(symbol) {
		return fmt.Errorf("expected %q, got %q", symbol, p.current().text)
	}
	return nil
}

func (p *expressionParser) parseTernary() (expressionNode, error) {
	condition, errCondition := p.parseOr()
	if errCondition != nil || !p.match("?") {
		return condition, errCondition
	}
	if condition.valueType() != expressionValueBool {
		return nil, errors.New("pricing ternary condition must be boolean")
	}
	whenTrue, errTrue := p.parseTernary()
	if errTrue != nil {
		return nil, errTrue
	}
	if errColon := p.expect(":"); errColon != nil {
		return nil, errColon
	}
	whenFalse, errFalse := p.parseTernary()
	if errFalse != nil {
		return nil, errFalse
	}
	if whenTrue.valueType() != whenFalse.valueType() {
		return nil, errors.New("pricing ternary branches must have the same type")
	}
	return expressionConditionalNode{condition: condition, whenTrue: whenTrue, whenFalse: whenFalse, kind: whenTrue.valueType()}, nil
}

func (p *expressionParser) parseOr() (expressionNode, error) {
	return p.parseBinary(p.parseAnd, []string{"||"}, expressionValueBool)
}
func (p *expressionParser) parseAnd() (expressionNode, error) {
	return p.parseBinary(p.parseComparison, []string{"&&"}, expressionValueBool)
}

func (p *expressionParser) parseComparison() (expressionNode, error) {
	left, errLeft := p.parseAddition()
	if errLeft != nil {
		return nil, errLeft
	}
	for isCurrentExpressionOperator(p.current(), "<", "<=", ">", ">=", "==") {
		op := p.advance().text
		right, errRight := p.parseAddition()
		if errRight != nil {
			return nil, errRight
		}
		if op == "==" {
			if left.valueType() != right.valueType() {
				return nil, errors.New("pricing equality operands must have the same type")
			}
		} else if left.valueType() != expressionValueNumber || right.valueType() != expressionValueNumber {
			return nil, errors.New("pricing comparison operands must be numbers")
		}
		left = expressionBinaryNode{op: op, left: left, right: right, kind: expressionValueBool}
	}
	return left, nil
}

func (p *expressionParser) parseAddition() (expressionNode, error) {
	return p.parseBinary(p.parseMultiplication, []string{"+"}, expressionValueNumber)
}
func (p *expressionParser) parseMultiplication() (expressionNode, error) {
	return p.parseBinary(p.parsePrimary, []string{"*"}, expressionValueNumber)
}

func (p *expressionParser) parseBinary(next func() (expressionNode, error), operators []string, required expressionValueType) (expressionNode, error) {
	left, errLeft := next()
	if errLeft != nil {
		return nil, errLeft
	}
	for isCurrentExpressionOperator(p.current(), operators...) {
		op := p.advance().text
		right, errRight := next()
		if errRight != nil {
			return nil, errRight
		}
		if left.valueType() != required || right.valueType() != required {
			return nil, fmt.Errorf("pricing operator %s has invalid operands", op)
		}
		kind := required
		if op == "&&" || op == "||" {
			kind = expressionValueBool
		}
		left = expressionBinaryNode{op: op, left: left, right: right, kind: kind}
	}
	return left, nil
}

func (p *expressionParser) parsePrimary() (expressionNode, error) {
	token := p.advance()
	switch token.kind {
	case expressionTokenNumber:
		value, errNumber := strconv.ParseFloat(token.text, 64)
		if errNumber != nil || math.IsInf(value, 0) || math.IsNaN(value) {
			return nil, errors.New("invalid pricing number")
		}
		return expressionLiteralNode{value: expressionValue{kind: expressionValueNumber, number: value}}, nil
	case expressionTokenString:
		return expressionLiteralNode{value: expressionValue{kind: expressionValueString, text: token.text}}, nil
	case expressionTokenIdentifier:
		if token.text == "true" {
			return expressionLiteralNode{value: expressionValue{kind: expressionValueBool, boolean: true}}, nil
		}
		if p.match("(") {
			return p.parseCall(token.text)
		}
		if !isAllowedPricingVariable(token.text) {
			return nil, fmt.Errorf("unknown pricing identifier %q", token.text)
		}
		return expressionVariableNode{name: token.text}, nil
	case expressionTokenSymbol:
		if token.text != "(" {
			return nil, fmt.Errorf("unexpected pricing token %q", token.text)
		}
		p.depth++
		if p.depth > maxPricingExpressionDepth {
			return nil, errors.New("pricing expression nesting is too deep")
		}
		node, errNode := p.parseTernary()
		p.depth--
		if errNode != nil {
			return nil, errNode
		}
		if errClose := p.expect(")"); errClose != nil {
			return nil, errClose
		}
		return node, nil
	default:
		return nil, errors.New("unexpected end of pricing expression")
	}
}

func (p *expressionParser) parseCall(name string) (expressionNode, error) {
	p.depth++
	if p.depth > maxPricingExpressionDepth {
		return nil, errors.New("pricing expression nesting is too deep")
	}
	defer func() { p.depth-- }()
	args := make([]expressionNode, 0, 2)
	if !p.match(")") {
		for {
			argument, errArgument := p.parseTernary()
			if errArgument != nil {
				return nil, errArgument
			}
			args = append(args, argument)
			if p.match(")") {
				break
			}
			if errComma := p.expect(","); errComma != nil {
				return nil, errComma
			}
		}
	}
	switch name {
	case "tier":
		if len(args) != 2 || args[0].valueType() != expressionValueString || args[1].valueType() != expressionValueNumber {
			return nil, errors.New("tier requires a label and numeric value")
		}
		label, okLabel := args[0].(expressionLiteralNode)
		if !okLabel || strings.TrimSpace(label.value.text) == "" {
			return nil, errors.New("tier label must be a non-empty string")
		}
		return expressionCallNode{name: name, args: args, kind: expressionValueNumber}, nil
	case "fixed":
		if len(args) != 1 || args[0].valueType() != expressionValueNumber {
			return nil, errors.New("fixed requires one numeric value")
		}
		return expressionCallNode{name: name, args: args, kind: expressionValueNumber}, nil
	case "param":
		if !singleLiteralStringArgument(args, "enable_thinking") {
			return nil, errors.New("param only supports enable_thinking")
		}
		return expressionCallNode{name: name, args: args, kind: expressionValueBool}, nil
	case "weekday", "hour":
		if !singleLiteralStringArgument(args, "UTC") {
			return nil, fmt.Errorf("%s only supports UTC", name)
		}
		return expressionCallNode{name: name, args: args, kind: expressionValueNumber}, nil
	default:
		return nil, fmt.Errorf("unsupported pricing function %q", name)
	}
}

func singleLiteralStringArgument(args []expressionNode, expected string) bool {
	if len(args) != 1 || args[0].valueType() != expressionValueString {
		return false
	}
	literal, okLiteral := args[0].(expressionLiteralNode)
	return okLiteral && literal.value.text == expected
}

func isAllowedPricingVariable(name string) bool {
	switch name {
	case "p", "cr", "cc", "cc1h", "cc12h", "c", "ai", "ao", "len", "image_count":
		return true
	default:
		return false
	}
}

func isCurrentExpressionOperator(token expressionToken, operators ...string) bool {
	if token.kind != expressionTokenSymbol {
		return false
	}
	for _, operator := range operators {
		if token.text == operator {
			return true
		}
	}
	return false
}
