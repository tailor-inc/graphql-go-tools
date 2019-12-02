package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/buger/jsonparser"
	"github.com/cespare/xxhash"
	"github.com/jensneuse/graphql-go-tools/internal/pkg/unsafebytes"
	"github.com/jensneuse/graphql-go-tools/pkg/ast"
	"github.com/jensneuse/graphql-go-tools/pkg/lexer/literal"
	"github.com/jensneuse/graphql-go-tools/pkg/lexer/runes"
	"github.com/valyala/fasttemplate"
	"io"
	"strconv"
	"strings"
	"sync"
)

type Executor struct {
	context      Context
	out          io.Writer
	err          error
	buffers      LockableBufferMap
	instructions []Instruction
}

type LockableBufferMap struct {
	sync.Mutex
	Buffers map[uint64]*bytes.Buffer
}

func NewExecutor() *Executor {
	return &Executor{
		buffers: LockableBufferMap{
			Buffers: map[uint64]*bytes.Buffer{},
		},
	}
}

type Instruction int

const (
	KeepStreamAlive Instruction = iota + 1
	CloseConnection
	CloseConnectionIfNotStream
)

func (e *Executor) Execute(ctx Context, node RootNode, w io.Writer) (instruction []Instruction, err error) {
	e.context = ctx
	e.out = w
	e.err = nil
	e.instructions = e.instructions[:0]
	var path string
	switch node.OperationType() {
	case ast.OperationTypeQuery:
		path = "query"
	case ast.OperationTypeMutation:
		path = "mutation"
	case ast.OperationTypeSubscription:
		path = "subscription"
	}
	e.resolveNode(node, nil, path, nil, true)
	return e.instructions, e.err
}

func (e *Executor) write(data []byte) {
	if e.err != nil {
		return
	}
	_, e.err = e.out.Write(data)
}

func (e *Executor) resolveNode(node Node, data []byte, path string, prefetch *sync.WaitGroup, shouldFetch bool) {
	switch node := node.(type) {
	case *Object:

		if data != nil && node.Path != nil {
			data, _, _, e.err = jsonparser.Get(data, node.Path...)
			if e.err == jsonparser.KeyPathNotFoundError {
				e.err = nil
				e.write(literal.NULL)
				return
			}
		}

		if shouldFetch && node.Fetch != nil {
			e.instructions = append(e.instructions, node.Fetch.Fetch(e.context, data, e, path, &e.buffers))
		}

		if prefetch != nil {
			prefetch.Done()
			return
		}

		if bytes.Equal(data, literal.NULL) {
			e.write(literal.NULL)
			return
		}
		e.write(literal.LBRACE)

		for i := 0; i < len(node.Fields); i++ {
			if node.Fields[i].Skip != nil {
				if node.Fields[i].Skip.Evaluate(e.context, data) {
					continue
				}
			}
			if i != 0 {
				e.write(literal.COMMA)
			}
			e.resolveNode(&node.Fields[i], data, path, nil, true)
		}
		e.write(literal.RBRACE)
	case *Field:
		path = path + "." + unsafebytes.BytesToString(node.Name)
		if node.HasResolver {
			buffer, ok := e.buffers.Buffers[xxhash.Sum64String(path)]
			if !ok {
				e.write(literal.QUOTE)
				e.write(node.Name)
				e.write(literal.QUOTE)
				e.write(literal.COLON)
				e.write(literal.NULL)
				return
			}
			data = buffer.Bytes()
		}
		e.write(literal.QUOTE)
		e.write(node.Name)
		e.write(literal.QUOTE)
		e.write(literal.COLON)
		if len(data) == 0 && !node.Value.HasResolvers() {
			e.write(literal.NULL)
			return
		}
		e.resolveNode(node.Value, data, path, nil, true)
	case *Value:
		if bytes.Equal(data, literal.NULL) {
			e.write(literal.NULL)
			return
		}
		if len(node.Path) == 0 {
			if node.QuoteValue {
				e.write(literal.QUOTE)
			}
			e.write(data)
			if node.QuoteValue {
				e.write(literal.QUOTE)
			}
			return
		}
		data, _, _, e.err = jsonparser.Get(data, node.Path...)
		if e.err == jsonparser.KeyPathNotFoundError {
			e.err = nil
			e.write(literal.NULL)
			return
		}
		if node.QuoteValue {
			e.write(literal.QUOTE)
		}
		e.write(data)
		if node.QuoteValue {
			e.write(literal.QUOTE)
		}
	case *List:
		if len(data) == 0 {
			e.write(literal.NULL)
			return
		}
		shouldPrefetch := false
		switch object := node.Value.(type) {
		case *Object:
			if object.Fetch != nil {
				shouldPrefetch = true
			}
		}
		var listItems [][]byte
		_, e.err = jsonparser.ArrayEach(data, func(value []byte, dataType jsonparser.ValueType, offset int, err error) {
			listItems = append(listItems, value)
		}, node.Path...)

		path = path + "."

		maxItems := len(listItems)
		if node.Filter != nil {
			switch filter := node.Filter.(type) {
			case *ListFilterFirstN:
				if maxItems > filter.FirstN {
					maxItems = filter.FirstN
				}
			}
		}

		if shouldPrefetch {
			wg := &sync.WaitGroup{}
			for i := 0; i < maxItems; i++ {
				wg.Add(1)
				go e.resolveNode(node.Value, listItems[i], path+strconv.Itoa(i), wg, true)
			}
			wg.Wait()
		}
		i := 0
		for i = 0; i < maxItems; i++ {
			if i == 0 {
				e.write(literal.LBRACK)
			} else {
				e.write(literal.COMMA)
			}
			e.resolveNode(node.Value, listItems[i], path+strconv.Itoa(i), nil, false)
		}
		if i == 0 || e.err == jsonparser.KeyPathNotFoundError {
			e.err = nil
			e.write(literal.LBRACK)
		}
		e.write(literal.RBRACK)
	}
}

func (e *Executor) ResolveArgs(args []Argument, data []byte) ResolvedArgs {

	args = append(args, e.context.ExtraArguments...)

	resolved := make(ResolvedArgs, len(args))
	for i := 0; i < len(args); i++ {
		switch arg := args[i].(type) {
		case *StaticVariableArgument:
			resolved[i].Key = arg.Name
			resolved[i].Value = arg.Value
		case *ObjectVariableArgument:
			resolved[i].Key = arg.Name
			resolved[i].Value, _, _, _ = jsonparser.Get(data, arg.Path...)
		case *ContextVariableArgument:
			resolved[i].Key = arg.Name
			resolved[i].Value = e.context.Variables[xxhash.Sum64(arg.VariableName)]
		case *ListArgument:
			resolved[i].Key = arg.Name
			listArgs := e.ResolveArgs(arg.Arguments, data)
			listValues := make(map[string]string, len(listArgs))
			for j := range listArgs {
				listValues[string(listArgs[j].Key)] = string(listArgs[j].Value)
			}
			resolved[i].Value, _ = json.Marshal(listValues)
		}
	}

	for i := range resolved {
		if !bytes.Contains(resolved[i].Value, literal.DOUBLE_LBRACE) {
			continue
		}
		t, err := fasttemplate.NewTemplate(string(resolved[i].Value), literal.DOUBLE_LBRACE_STR, literal.DOUBLE_RBRACE_STR)
		if err != nil {
			continue
		}
		value := t.ExecuteFuncString(func(w io.Writer, tag string) (i int, e error) {
			tag = strings.TrimFunc(tag, func(r rune) bool {
				return r == runes.SPACE || r == runes.TAB || r == runes.LINETERMINATOR
			})
			if strings.Count(tag, ".") == 1 {
				tag = strings.TrimPrefix(tag, ".")
				tagBytes := []byte(tag)
				for j := range resolved {
					if bytes.Equal(resolved[j].Key, tagBytes) {
						return w.Write(resolved[j].Value)
					}
				}
			}
			for j := range resolved {
				key := string(resolved[j].Key)
				if strings.HasPrefix(tag,".") && !strings.HasPrefix(key,"."){
					key = "." + key
				}
				if !strings.HasPrefix(tag, key) {
					continue
				}
				key = strings.TrimPrefix(tag, key)
				if key == "" {
					return w.Write(resolved[j].Value)
				}
				key = strings.TrimPrefix(key, ".")
				keys := strings.Split(key, ".")
				value, _, _, _ := jsonparser.Get(resolved[j].Value, keys...)
				return w.Write(value)
			}
			return w.Write([]byte("{{ " + tag + " }}"))
		})
		resolved[i].Value = []byte(value)
	}

	resolved.Filter(func(i int) (keep bool) {
		return !bytes.HasPrefix(resolved[i].Key, literal.DOT)
	})

	return resolved
}

const (
	ObjectKind NodeKind = iota + 1
	FieldKind
	ListKind
	ValueKind
)

type NodeKind int

type Node interface {
	Kind() NodeKind
	HasResolvers() bool
}

type RootNode interface {
	Node
	OperationType() ast.OperationType
}

type Context struct {
	context.Context
	Variables      Variables
	ExtraArguments []Argument
}

type Variables map[uint64][]byte

type Argument interface {
	ArgName() []byte
}

type ResolvedArgument struct {
	Key   []byte
	Value []byte
}

type ResolvedArgs []ResolvedArgument

func (r *ResolvedArgs) Filter(condition func(i int) (keep bool)) {
	n := 0
	for i := range *r {
		if condition(i) {
			(*r)[n] = (*r)[i]
			n++
		}
	}
	*r = (*r)[:n]
}

var (
	keys = []byte("abcdefghijklmnopqrstuvwxyz")
)

func (r ResolvedArgs) NextKey() []byte {
	for i := 0; i < len(keys); i++ {
		if r.ByKey(keys[i:i+1]) == nil {
			return keys[i : i+1]
		}
	}
	return nil
}

func (r ResolvedArgs) ByKey(key []byte) []byte {
	for i := 0; i < len(r); i++ {
		if bytes.Equal(r[i].Key, key) {
			return r[i].Value
		}
	}
	return nil
}

func (r ResolvedArgs) Dump() []string {
	out := make([]string, len(r))
	for i := range r {
		out[i] = string(r[i].Key) + "=" + string(r[i].Value)
	}
	return out
}

type ContextVariableArgument struct {
	Name         []byte
	VariableName []byte
}

func (c *ContextVariableArgument) ArgName() []byte {
	return c.Name
}

type ObjectVariableArgument struct {
	Name []byte
	Path []string
}

func (o *ObjectVariableArgument) ArgName() []byte {
	return o.Name
}

type StaticVariableArgument struct {
	Name  []byte
	Value []byte
}

func (s *StaticVariableArgument) ArgName() []byte {
	return s.Name
}

type ListArgument struct {
	Name      []byte
	Arguments []Argument
}

func (l ListArgument) ArgName() []byte {
	return l.Name
}

type Object struct {
	Fields        []Field
	Path          []string
	Fetch         Fetch
	operationType ast.OperationType
}

func (o *Object) OperationType() ast.OperationType {
	return o.operationType
}

type ArgsResolver interface {
	ResolveArgs(args []Argument, data []byte) ResolvedArgs
}

type Fetch interface {
	Fetch(ctx Context, data []byte, argsResolver ArgsResolver, suffix string, buffers *LockableBufferMap) Instruction
}

type SingleFetch struct {
	Source     *DataSourceInvocation
	BufferName string
}

func (s *SingleFetch) Fetch(ctx Context, data []byte, argsResolver ArgsResolver, path string, buffers *LockableBufferMap) Instruction {
	bufferName := path + "." + s.BufferName
	hash := xxhash.Sum64String(bufferName)
	buffers.Lock()
	buffer, exists := buffers.Buffers[hash]
	buffers.Unlock()
	if !exists {
		buffer = bytes.NewBuffer(make([]byte, 0, 1024))
		buffers.Lock()
		buffers.Buffers[hash] = buffer
		buffers.Unlock()
	} else {
		buffer.Reset()
	}
	return s.Source.DataSource.Resolve(ctx, argsResolver.ResolveArgs(s.Source.Args, data), buffer)
}

type SerialFetch struct {
	Fetches []Fetch
}

func (s *SerialFetch) Fetch(ctx Context, data []byte, argsResolver ArgsResolver, suffix string, buffers *LockableBufferMap) Instruction {
	for i := 0; i < len(s.Fetches); i++ {
		s.Fetches[i].Fetch(ctx, data, argsResolver, suffix, buffers)
	}
	return CloseConnection
}

type ParallelFetch struct {
	wg      sync.WaitGroup
	Fetches []Fetch
}

func (p *ParallelFetch) Fetch(ctx Context, data []byte, argsResolver ArgsResolver, suffix string, buffers *LockableBufferMap) Instruction {
	for i := 0; i < len(p.Fetches); i++ {
		p.wg.Add(1)
		go func(fetch Fetch, ctx Context, data []byte, argsResolver ArgsResolver) {
			fetch.Fetch(ctx, data, argsResolver, suffix, buffers)
			p.wg.Done()
		}(p.Fetches[i], ctx, data, argsResolver)
	}
	p.wg.Wait()
	return CloseConnection
}

func (o *Object) HasResolvers() bool {
	for i := 0; i < len(o.Fields); i++ {
		if o.Fields[i].HasResolvers() {
			return true
		}
	}
	return false
}

func (*Object) Kind() NodeKind {
	return ObjectKind
}

type BooleanCondition interface {
	Evaluate(ctx Context, data []byte) bool
}

type Field struct {
	Name        []byte
	Value       Node
	Skip        BooleanCondition
	HasResolver bool
}

func (f *Field) HasResolvers() bool {
	return f.HasResolver || f.Value.HasResolvers()
}

type IfEqual struct {
	Left, Right Argument
}

func (i *IfEqual) Evaluate(ctx Context, data []byte) bool {
	var left []byte
	var right []byte

	switch value := i.Left.(type) {
	case *ContextVariableArgument:
		left = ctx.Variables[xxhash.Sum64(value.VariableName)]
	case *ObjectVariableArgument:
		left, _, _, _ = jsonparser.Get(data, value.Path...)
	case *StaticVariableArgument:
		left = value.Value
	}

	switch value := i.Right.(type) {
	case *ContextVariableArgument:
		right = ctx.Variables[xxhash.Sum64(value.VariableName)]
	case *ObjectVariableArgument:
		right, _, _, _ = jsonparser.Get(data, value.Path...)
	case *StaticVariableArgument:
		right = value.Value
	}

	return bytes.Equal(left, right)
}

type IfNotEqual struct {
	Left, Right Argument
}

func (i *IfNotEqual) Evaluate(ctx Context, data []byte) bool {
	equal := IfEqual{
		Left:  i.Left,
		Right: i.Right,
	}
	return !equal.Evaluate(ctx, data)
}

func (*Field) Kind() NodeKind {
	return FieldKind
}

type Value struct {
	Path       []string
	QuoteValue bool
}

func (value *Value) HasResolvers() bool {
	return false
}

func (*Value) Kind() NodeKind {
	return ValueKind
}

type List struct {
	Path   []string
	Value  Node
	Filter ListFilter
}

func (l *List) HasResolvers() bool {
	return l.Value.HasResolvers()
}

func (*List) Kind() NodeKind {
	return ListKind
}

type ListFilter interface {
	Kind() ListFilterKind
}

type ListFilterKind int

const (
	ListFilterKindFirstN ListFilterKind = iota + 1
)

type ListFilterFirstN struct {
	FirstN int
}

func (_ ListFilterFirstN) Kind() ListFilterKind {
	return ListFilterKindFirstN
}

type DataSourceInvocation struct {
	Args       []Argument
	DataSource DataSource
}