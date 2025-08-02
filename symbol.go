package main

import (
	"go/ast"
	"go/parser"
	"go/token"
)

type SymbolKind string

const (
	SymbolFunc   SymbolKind = "func"
	SymbolType   SymbolKind = "type"
	SymbolVar    SymbolKind = "var"
	SymbolConst  SymbolKind = "const"
	SymbolImport SymbolKind = "import"
)

type Symbol struct {
	Name     string     // e.g., "Foo"
	Kind     SymbolKind // e.g., "func", "type"
	File     string     // absolute or relative path
	Line     int        // line number
	Column   int        // optional, for precision
	Receiver string     // for method: struct name
}

// type SymbolIndex struct {
// 	Symbols       map[string][]Symbol // name → list of symbols
// 	fileToSymbols map[string][]string
//  sync.RWMutex
// }

func ParseSymbol(filename string) (map[string][]Symbol, error) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		return nil, err
	}

	index := make(map[string][]Symbol)

	ast.Inspect(node, func(n ast.Node) bool {
		switch node := n.(type) {

		case *ast.FuncDecl:
			pos := fset.Position(node.Pos())
			receiver := ""
			if node.Recv != nil && len(node.Recv.List) > 0 {
				typ := node.Recv.List[0].Type
				switch t := typ.(type) {
				case *ast.Ident:
					receiver = t.Name
				case *ast.StarExpr:
					if ident, ok := t.X.(*ast.Ident); ok {
						receiver = ident.Name
					}
				}
			}

			sym := Symbol{
				Name:     node.Name.Name,
				Kind:     SymbolFunc,
				File:     filename,
				Line:     pos.Line,
				Column:   pos.Column,
				Receiver: receiver,
			}
			index[sym.Name] = append(index[sym.Name], sym)

		case *ast.GenDecl:
			for _, spec := range node.Specs {
				switch ts := spec.(type) {
				case *ast.TypeSpec:
					pos := fset.Position(ts.Pos())
					sym := Symbol{
						Name:   ts.Name.Name,
						Kind:   SymbolType,
						File:   filename,
						Line:   pos.Line,
						Column: pos.Column,
					}
					index[sym.Name] = append(index[sym.Name], sym)

				case *ast.ValueSpec:
					for _, name := range ts.Names {
						pos := fset.Position(name.Pos())
						kind := SymbolVar
						if node.Tok == token.CONST {
							kind = SymbolConst
						}
						sym := Symbol{
							Name:   name.Name,
							Kind:   kind,
							File:   filename,
							Line:   pos.Line,
							Column: pos.Column,
						}
						index[sym.Name] = append(index[sym.Name], sym)
					}
				}
			}
		}

		return true
	})

	return index, nil
}
