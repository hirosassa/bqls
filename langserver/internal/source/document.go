package source

import (
	"bufio"
	"context"
	"fmt"
	"strings"

	"cloud.google.com/go/bigquery"
	"github.com/goccy/go-zetasql"
	"github.com/goccy/go-zetasql/ast"
	rast "github.com/goccy/go-zetasql/resolved_ast"
	"github.com/kitagry/bqls/langserver/internal/lsp"
)

func (p *Project) TermDocument(uri string, position lsp.Position) ([]lsp.MarkedString, error) {
	ctx := context.Background()
	sql := p.cache.Get(uri)

	termOffset := positionToByteOffset(sql.RawText, position)
	var targetNode *ast.PathExpressionNode
	ast.Walk(sql.Node, func(n ast.Node) error {
		node, ok := n.(*ast.PathExpressionNode)
		if !ok {
			return nil
		}
		lRange := node.ParseLocationRange()
		startOffset := lRange.Start().ByteOffset()
		endOffset := lRange.End().ByteOffset()
		if startOffset <= termOffset && termOffset <= endOffset {
			targetNode = node
		}
		return nil
	})

	if targetNode == nil {
		return nil, nil
	}

	// lookup table metadata
	if targetNode, ok := lookupNode[*ast.TablePathExpressionNode](targetNode); ok {
		result, err := p.createTableMarkedString(ctx, targetNode)
		if err != nil {
			return nil, fmt.Errorf("failed to create table marked string: %w", err)
		}
		if len(result) > 0 {
			return result, nil
		}
	}

	outputs, err := p.analyzeStatement(sql)
	if err != nil {
		return nil, fmt.Errorf("failed to analyze statement: %w", err)
	}

	if node, err := p.getGetStructFieldNode(outputs, termOffset); err == nil {
		return []lsp.MarkedString{
			{
				Language: "markdown",
				Value:    node.Type().DebugString(false),
			},
		}, nil
	}

	if selectColumnNode, ok := lookupNode[*ast.SelectColumnNode](targetNode); ok {
		c, err := p.getSelectColumnNodeToAnalyzedOutputCoumnNode(outputs, selectColumnNode, termOffset)
		if err != nil {
			return nil, fmt.Errorf("failed to get column info: %w", err)
		}

		column := c.Column()
		if column == nil {
			return nil, fmt.Errorf("failed to find column: %v", c)
		}

		tableMetadata, err := p.getTableMetadataFromPath(ctx, column.TableNameID())
		if err != nil {
			return nil, fmt.Errorf("failed to get table metadata: %w", err)
		}

		for _, c := range tableMetadata.Schema {
			if column.Name() == c.Name {
				return []lsp.MarkedString{
					{
						Language: "markdown",
						Value:    fmt.Sprintf("%s: %s\n%s", c.Name, c.Type, c.Description),
					},
				}, nil
			}
		}
	}

	term, err := p.getColumnRefNode(outputs, termOffset)
	if err == nil {
		column := term.Column()
		if column == nil {
			return nil, fmt.Errorf("failed to find term: %v", term)
		}

		tableMetadata, err := p.getTableMetadataFromPath(ctx, column.TableNameID())
		if err != nil {
			// cannot find table metadata
			return []lsp.MarkedString{
				{
					Language: "markdown",
					Value:    fmt.Sprintf("%s: %s", column.Name(), column.Type()),
				},
			}, nil
		}

		for _, c := range tableMetadata.Schema {
			if column.Name() == c.Name {
				return []lsp.MarkedString{
					{
						Language: "markdown",
						Value:    fmt.Sprintf("%s: %s\n%s", c.Name, c.Type, c.Description),
					},
				}, nil
			}
		}
	}
	return nil, nil
}

func (p *Project) createTableMarkedString(ctx context.Context, node *ast.TablePathExpressionNode) ([]lsp.MarkedString, error) {
	pathExpr := node.PathExpr()
	if pathExpr == nil {
		return nil, nil
	}
	pathNames := make([]string, len(pathExpr.Names()))
	for i, n := range node.PathExpr().Names() {
		pathNames[i] = n.Name()
	}
	targetTable, err := p.getTableMetadataFromPath(ctx, strings.Join(pathNames, "."))
	if err != nil {
		return nil, fmt.Errorf("failed to get table metadata: %w", err)
	}

	columns := make([]string, len(targetTable.Schema))
	for i, c := range targetTable.Schema {
		columns[i] = fmt.Sprintf("* %s: %s %s", c.Name, string(c.Type), c.Description)
	}

	return buildBigQueryTableMetadataMarkedString(targetTable)
}

func (p *Project) getSelectColumnNodeToAnalyzedOutputCoumnNode(outputs []*zetasql.AnalyzerOutput, column *ast.SelectColumnNode, termOffset int) (*rast.OutputColumnNode, error) {
	for _, output := range outputs {
		children := output.Statement().ChildNodes()
		var tables []*rast.TableScanNode
		rast.Walk(output.Statement(), func(n rast.Node) error {
			t, ok := n.(*rast.TableScanNode)
			if !ok {
				return nil
			}
			tables = append(tables, t)
			return nil
		})

		for _, child := range children {
			outputColumn, ok := child.(*rast.OutputColumnNode)
			if !ok {
				continue
			}

			columnName, ok := getSelectColumnName(column)
			if !ok {
				continue
			}

			for _, t := range tables {
				if strings.HasPrefix(columnName, fmt.Sprintf("%s.", t.Alias())) {
					columnName = strings.TrimLeft(columnName, fmt.Sprintf("%s.", t.Alias()))
				}
			}

			if outputColumn.Name() == columnName {
				return outputColumn, nil
			}
		}
	}
	return nil, fmt.Errorf("failed to find column info")
}

func (p *Project) getColumnRefNode(outputs []*zetasql.AnalyzerOutput, termOffset int) (*rast.ColumnRefNode, error) {
	for _, output := range outputs {
		var targetNode *rast.ColumnRefNode
		rast.Walk(output.Statement(), func(n rast.Node) error {
			node, ok := n.(*rast.ColumnRefNode)
			if !ok {
				return nil
			}

			lRange := node.ParseLocationRange()
			if lRange == nil {
				return nil
			}

			startOffset := lRange.Start().ByteOffset()
			endOffset := lRange.End().ByteOffset()
			if startOffset <= termOffset && termOffset <= endOffset {
				targetNode = node
			}
			return nil
		})

		if targetNode != nil {
			return targetNode, nil
		}
	}
	return nil, fmt.Errorf("failed to find column info")
}

func (p *Project) getGetStructFieldNode(outputs []*zetasql.AnalyzerOutput, termOffset int) (*rast.GetStructFieldNode, error) {
	for _, output := range outputs {
		var targetNode *rast.GetStructFieldNode
		rast.Walk(output.Statement(), func(n rast.Node) error {
			node, ok := n.(*rast.GetStructFieldNode)
			if !ok {
				return nil
			}

			lRange := node.ParseLocationRange()
			if lRange == nil {
				return nil
			}

			startOffset := lRange.Start().ByteOffset()
			endOffset := lRange.End().ByteOffset()
			if startOffset <= termOffset && termOffset <= endOffset {
				targetNode = node
			}
			return nil
		})

		if targetNode != nil {
			return targetNode, nil
		}
	}
	return nil, fmt.Errorf("failed to find column info")
}

func getSelectColumnName(targetNode *ast.SelectColumnNode) (string, bool) {
	alias := targetNode.Alias()
	if alias != nil {
		return alias.Name(), true
	}

	path, ok := targetNode.Expression().(*ast.PathExpressionNode)
	if !ok {
		return "", false
	}

	names := make([]string, len(path.Names()))
	for i, t := range path.Names() {
		names[i] = t.Name()
	}
	return strings.Join(names, "."), true
}

func buildBigQueryTableMetadataMarkedString(metadata *bigquery.TableMetadata) ([]lsp.MarkedString, error) {
	resultStr := fmt.Sprintf("## %s", metadata.FullID)

	if len(metadata.Description) > 0 {
		resultStr += fmt.Sprintf("\n%s", metadata.Description)
	}

	resultStr += fmt.Sprintf("\ncreated at %s", metadata.CreationTime.Format("2006-01-02 15:04:05"))
	// If cache the metadata, we should delete last modified time because it is confusing.
	resultStr += fmt.Sprintf("\nlast modified at %s", metadata.LastModifiedTime.Format("2006-01-02 15:04:05"))

	schemaJson, err := metadata.Schema.ToJSONFields()
	if err != nil {
		return nil, fmt.Errorf("failed to convert schema to json: %w", err)
	}

	return []lsp.MarkedString{
		{
			Language: "markdown",
			Value:    resultStr,
		},
		{
			Language: "json",
			Value:    string(schemaJson),
		},
	}, nil
}

func positionToByteOffset(sql string, position lsp.Position) int {
	buf := bufio.NewScanner(strings.NewReader(sql))
	buf.Split(bufio.ScanLines)

	var offset int
	for i := 0; i < position.Line; i++ {
		buf.Scan()
		offset += len([]byte(buf.Text())) + 1
	}
	offset += position.Character
	return offset
}

type astNode interface {
	*ast.TablePathExpressionNode | *ast.PathExpressionNode | *ast.SelectColumnNode
}

func lookupNode[T astNode](n ast.Node) (T, bool) {
	if n == nil {
		return nil, false
	}

	result, ok := n.(T)
	if ok {
		return result, true
	}

	return lookupNode[T](n.Parent())
}