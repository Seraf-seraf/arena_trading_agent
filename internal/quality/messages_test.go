package quality_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// TestRussianContextualDiagnostics protects the operator-facing diagnostics
// contract: application errors and structured log messages are Russian, while
// every function that creates them declares its own stable methodCtx.
func TestRussianContextualDiagnostics(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("quality.TestRussianContextualDiagnostics: не удалось определить путь к тесту")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	files := make([]string, 0)
	for _, directory := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") &&
				!strings.HasSuffix(entry.Name(), "_test.go") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("quality.TestRussianContextualDiagnostics: не удалось перечислить исходники: %v", err)
		}
	}

	for _, path := range files {
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Errorf("quality.TestRussianContextualDiagnostics: не удалось разобрать %s: %v", path, err)
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch function := node.(type) {
			case *ast.FuncDecl:
				if function.Body != nil {
					checkDiagnosticFunction(
						t,
						fileSet,
						path,
						function.Name.Name,
						function.Pos(),
						function.Body,
					)
				}
			case *ast.FuncLit:
				checkDiagnosticFunction(
					t,
					fileSet,
					path,
					"анонимная функция",
					function.Pos(),
					function.Body,
				)
			}
			return true
		})
	}
}

func TestAnonymousDiagnosticRequiresOwnMethodContext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		source            string
		wantMethodContext bool
	}{
		{
			name: "outer context is not inherited",
			source: `package fixture
func outer() {
	const methodCtx = "fixture.outer"
	_ = func() error {
		return fmt.Errorf("русская ошибка")
	}
}`,
			wantMethodContext: false,
		},
		{
			name: "alternate context name is rejected",
			source: `package fixture
func outer() {
	_ = func() error {
		const closureMethodCtx = "fixture.outer.closure"
		return fmt.Errorf("русская ошибка")
	}
}`,
			wantMethodContext: false,
		},
		{
			name: "local exact context is accepted",
			source: `package fixture
func outer() {
	_ = func() error {
		const methodCtx = "fixture.outer.closure"
		return fmt.Errorf("русская ошибка")
	}
}`,
			wantMethodContext: true,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			fileSet := token.NewFileSet()
			file, err := parser.ParseFile(fileSet, "fixture.go", test.source, 0)
			if err != nil {
				t.Fatalf("quality.TestAnonymousDiagnosticRequiresOwnMethodContext: не удалось разобрать пример: %v", err)
			}
			var anonymous *ast.FuncLit
			ast.Inspect(file, func(node ast.Node) bool {
				if function, ok := node.(*ast.FuncLit); ok {
					anonymous = function
					return false
				}
				return anonymous == nil
			})
			if anonymous == nil {
				t.Fatal("quality.TestAnonymousDiagnosticRequiresOwnMethodContext: анонимная функция не найдена")
			}
			if diagnostics := directDiagnosticMessages(anonymous.Body); len(diagnostics) != 1 {
				t.Fatalf(
					"quality.TestAnonymousDiagnosticRequiresOwnMethodContext: получено диагностик %d, ожидалась 1",
					len(diagnostics),
				)
			}
			if got := hasMethodContext(anonymous.Body); got != test.wantMethodContext {
				t.Errorf(
					"quality.TestAnonymousDiagnosticRequiresOwnMethodContext: наличие контекста = %t, ожидалось %t",
					got,
					test.wantMethodContext,
				)
			}
		})
	}
}

type diagnosticMessage struct {
	expression ast.Expr
	name       string
}

func checkDiagnosticFunction(
	t *testing.T,
	fileSet *token.FileSet,
	path string,
	name string,
	position token.Pos,
	body *ast.BlockStmt,
) {
	t.Helper()

	diagnostics := directDiagnosticMessages(body)
	for _, diagnostic := range diagnostics {
		checkRussianExpression(t, fileSet, path, diagnostic)
	}
	if len(diagnostics) == 0 || hasMethodContext(body) {
		return
	}
	sourcePosition := fileSet.Position(position)
	t.Errorf(
		"quality.TestRussianContextualDiagnostics: %s:%d: функция %s создаёт ошибку или лог без локального `const methodCtx = \"...\"`",
		path,
		sourcePosition.Line,
		name,
	)
}

func directDiagnosticMessages(body *ast.BlockStmt) []diagnosticMessage {
	messages := make([]diagnosticMessage, 0)
	ast.Inspect(body, func(node ast.Node) bool {
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}
		switch value := node.(type) {
		case *ast.CallExpr:
			if message, ok := diagnosticCallMessage(value); ok {
				messages = append(messages, message)
			}
		case *ast.AssignStmt:
			for index, left := range value.Lhs {
				if index >= len(value.Rhs) || !isDiagnosticField(left) {
					continue
				}
				if expression, ok := constructedMessage(value.Rhs[index]); ok {
					messages = append(messages, diagnosticMessage{
						expression: expression,
						name:       "присваивание диагностического поля",
					})
				}
			}
		case *ast.KeyValueExpr:
			if !isDiagnosticField(value.Key) {
				break
			}
			if expression, ok := constructedMessage(value.Value); ok {
				messages = append(messages, diagnosticMessage{
					expression: expression,
					name:       "инициализация диагностического поля",
				})
			}
		}
		return true
	})
	return messages
}

func diagnosticCallMessage(call *ast.CallExpr) (diagnosticMessage, bool) {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return diagnosticMessage{}, false
	}
	switch {
	case isErrorConstructor(selector):
		return callMessage(selector, call, 0)
	case isHTTPError(selector):
		return callMessage(selector, call, 1)
	case isStandardErrorOutput(selector, call):
		return callMessage(selector, call, 1)
	case isStructuredLogCall(selector, call):
		return callMessage(selector, call, 0)
	default:
		return diagnosticMessage{}, false
	}
}

func callMessage(
	selector *ast.SelectorExpr,
	call *ast.CallExpr,
	index int,
) (diagnosticMessage, bool) {
	if index >= len(call.Args) {
		return diagnosticMessage{}, false
	}
	return diagnosticMessage{
		expression: call.Args[index],
		name:       expressionName(selector.X) + "." + selector.Sel.Name,
	}, true
}

func isErrorConstructor(selector *ast.SelectorExpr) bool {
	identifier, ok := selector.X.(*ast.Ident)
	if !ok {
		return false
	}
	return identifier.Name == "fmt" && selector.Sel.Name == "Errorf" ||
		identifier.Name == "errors" && selector.Sel.Name == "New"
}

func isHTTPError(selector *ast.SelectorExpr) bool {
	identifier, ok := selector.X.(*ast.Ident)
	return ok && identifier.Name == "http" && selector.Sel.Name == "Error"
}

func isStandardErrorOutput(selector *ast.SelectorExpr, call *ast.CallExpr) bool {
	identifier, ok := selector.X.(*ast.Ident)
	if !ok || identifier.Name != "fmt" || selector.Sel.Name != "Fprintf" ||
		len(call.Args) < 2 {
		return false
	}
	switch writer := call.Args[0].(type) {
	case *ast.Ident:
		return writer.Name == "stderr"
	case *ast.SelectorExpr:
		pkg, ok := writer.X.(*ast.Ident)
		return ok && pkg.Name == "os" && writer.Sel.Name == "Stderr"
	default:
		return false
	}
}

func isStructuredLogCall(selector *ast.SelectorExpr, call *ast.CallExpr) bool {
	if len(call.Args) == 0 || isHTTPError(selector) {
		return false
	}
	switch selector.Sel.Name {
	case "Debug", "Info", "Warn", "Error":
		return true
	default:
		return false
	}
}

func isDiagnosticField(expression ast.Expr) bool {
	var name string
	switch value := expression.(type) {
	case *ast.Ident:
		name = value.Name
	case *ast.SelectorExpr:
		name = value.Sel.Name
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return false
		}
		decoded, err := strconv.Unquote(value.Value)
		if err != nil {
			return false
		}
		name = decoded
	default:
		return false
	}
	switch strings.ToLower(name) {
	case "error", "failure", "message", "reason":
		return true
	default:
		return false
	}
}

func constructedMessage(expression ast.Expr) (ast.Expr, bool) {
	switch value := expression.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return nil, false
		}
		decoded, err := strconv.Unquote(value.Value)
		return value, err == nil && decoded != ""
	case *ast.CallExpr:
		selector, ok := value.Fun.(*ast.SelectorExpr)
		if !ok || len(value.Args) == 0 {
			return nil, false
		}
		identifier, ok := selector.X.(*ast.Ident)
		if ok && identifier.Name == "fmt" && selector.Sel.Name == "Sprintf" {
			return value.Args[0], true
		}
	}
	return nil, false
}

func checkRussianExpression(
	t *testing.T,
	fileSet *token.FileSet,
	path string,
	diagnostic diagnosticMessage,
) {
	t.Helper()

	russian := false
	ast.Inspect(diagnostic.expression, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			position := fileSet.Position(literal.Pos())
			t.Errorf(
				"quality.TestRussianContextualDiagnostics: %s:%d: некорректный строковый литерал: %v",
				path,
				position.Line,
				err,
			)
			return false
		}
		for _, character := range value {
			if unicode.In(character, unicode.Cyrillic) {
				russian = true
				return false
			}
		}
		return true
	})
	if russian {
		return
	}
	position := fileSet.Position(diagnostic.expression.Pos())
	t.Errorf(
		"quality.TestRussianContextualDiagnostics: %s:%d: сообщение %s не содержит русского текста в исходном выражении",
		path,
		position.Line,
		diagnostic.name,
	)
}

func hasMethodContext(body *ast.BlockStmt) bool {
	for _, statement := range body.List {
		declaration, ok := statement.(*ast.DeclStmt)
		if !ok {
			continue
		}
		general, ok := declaration.Decl.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, specification := range general.Specs {
			value, ok := specification.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for index, name := range value.Names {
				if name.Name != "methodCtx" || index >= len(value.Values) {
					continue
				}
				literal, ok := value.Values[index].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					continue
				}
				decoded, err := strconv.Unquote(literal.Value)
				if err == nil && strings.TrimSpace(decoded) != "" {
					return true
				}
			}
		}
	}
	return false
}

func expressionName(expression ast.Expr) string {
	if identifier, ok := expression.(*ast.Ident); ok {
		return identifier.Name
	}
	return "logger"
}
