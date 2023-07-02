package source

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"strings"

	"cloud.google.com/go/bigquery"
	"github.com/goccy/go-json"
	"github.com/goccy/go-yaml"
	"github.com/goccy/go-zetasql"
	"github.com/goccy/go-zetasql/ast"
	rast "github.com/goccy/go-zetasql/resolved_ast"
	"github.com/goccy/go-zetasql/types"
	"github.com/kitagry/bqls/langserver/internal/lsp"
)

func (p *Project) TermDocument(uri string, position lsp.Position) ([]lsp.MarkedString, error) {
	ctx := context.Background()
	sql := p.cache.Get(uri)
	parsedFile := p.ParseFile(uri, sql.RawText)

	termOffset := positionToByteOffset(sql.RawText, position)
	termOffset = parsedFile.fixTermOffsetForNode(termOffset)
	targetNode, ok := searchAstNode[*ast.PathExpressionNode](parsedFile.Node, termOffset)
	if !ok {
		p.logger.Debug("not found target node")
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

	output, ok := parsedFile.FindTargetAnalyzeOutput(termOffset)
	if !ok {
		return nil, nil
	}

	if node, ok := searchResolvedAstNode[*rast.FunctionCallNode](output, termOffset); ok {
		sigs := make([]string, 0, len(node.Function().Signatures()))
		for _, sig := range node.Function().Signatures() {
			sigs = append(sigs, sig.DebugString(node.Function().SQLName(), true))
		}
		return []lsp.MarkedString{
			{
				Language: "markdown",
				Value:    fmt.Sprintf("## %s\n\n%s", node.Function().SQLName(), strings.Join(sigs, "\n")),
			},
		}, nil
	}

	if node, ok := searchResolvedAstNode[*rast.GetStructFieldNode](output, termOffset); ok {
		return []lsp.MarkedString{
			{
				Language: "markdown",
				Value:    node.Type().TypeName(types.ProductExternal),
			},
		}, nil
	}

	if term, ok := searchResolvedAstNode[*rast.ColumnRefNode](output, termOffset); ok {
		column := term.Column()
		if column == nil {
			return nil, fmt.Errorf("failed to find term: %v", term)
		}

		tableMetadata, err := p.getTableMetadataFromPath(ctx, column.TableName())
		if err != nil {
			// cannot find table metadata
			return []lsp.MarkedString{
				{
					Language: "markdown",
					Value:    fmt.Sprintf("%s: %s", column.Name(), column.Type().TypeName(types.ProductExternal)),
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

	if selectColumnNode, ok := lookupNode[*ast.SelectColumnNode](targetNode); ok {
		column, err := p.getSelectColumnNodeToAnalyzedOutputCoumnNode(output, selectColumnNode, termOffset)
		if err != nil {
			return nil, fmt.Errorf("failed to get column info: %w", err)
		}

		tableMetadata, err := p.getTableMetadataFromPath(ctx, column.TableName())
		if err != nil {
			return []lsp.MarkedString{
				{
					Language: "markdown",
					Value:    fmt.Sprintf("%s: %s", column.Name(), column.Type().TypeName(types.ProductExternal)),
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
	tablePath, ok := createTableNameFromTablePathExpressionNode(node)
	if !ok {
		return nil, nil
	}
	targetTable, err := p.getTableMetadataFromPath(ctx, tablePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get table metadata: %w", err)
	}

	columns := make([]string, len(targetTable.Schema))
	for i, c := range targetTable.Schema {
		columns[i] = fmt.Sprintf("* %s: %s %s", c.Name, string(c.Type), c.Description)
	}

	return buildBigQueryTableMetadataMarkedString(targetTable)
}

func (p *Project) getSelectColumnNodeToAnalyzedOutputCoumnNode(output *zetasql.AnalyzerOutput, column *ast.SelectColumnNode, termOffset int) (*rast.Column, error) {
	scanNodes := make([]rast.ScanNode, 0)
	rast.Walk(output.Statement(), func(n rast.Node) error {
		t, ok := n.(rast.ScanNode)
		if !ok {
			return nil
		}
		scanNodes = append(scanNodes, t)
		return nil
	})

	mostNarrowWidth := math.MaxInt
	var targetScanNode rast.ScanNode
	for _, node := range scanNodes {
		lrange := node.ParseLocationRange()
		if lrange == nil {
			continue
		}

		width := lrange.End().ByteOffset() - lrange.Start().ByteOffset()
		if width < mostNarrowWidth {
			targetScanNode = node
		}
	}

	refNames := make([]string, 0)
	tmpScanNode := targetScanNode
	for tmpScanNode != nil {
		fmt.Printf("%T\n", tmpScanNode)
		switch n := tmpScanNode.(type) {
		case *rast.ProjectScanNode:
			tmpScanNode = n.InputScan()
		case *rast.WithScanNode:
			tmpScanNode = n.Query()
		case *rast.OrderByScanNode:
			tmpScanNode = n.InputScan()
		case *rast.TableScanNode:
			if n.Alias() != "" {
				refNames = append(refNames, n.Alias())
			}
			tmpScanNode = nil
		case *rast.WithRefScanNode:
			refNames = append(refNames, n.WithQueryName())
			tmpScanNode = nil
		default:
			p.logger.Debugf("Unsupported type: %T", n)
			tmpScanNode = nil
		}
	}

	columnName, ok := getSelectColumnName(column)
	if !ok {
		return nil, fmt.Errorf("failed getSelectColumnName: %s", column.DebugString(0))
	}

	fmt.Println(refNames)
	// remove table prefix
	for _, refName := range refNames {
		tablePrefix := fmt.Sprintf("%s.", refName)
		if strings.HasPrefix(columnName, tablePrefix) {
			columnName = strings.TrimPrefix(columnName, tablePrefix)
		}
	}

	for _, c := range targetScanNode.ColumnList() {
		fmt.Println(columnName, c.Name())
		if c.Name() == columnName {
			return c, nil
		}
	}
	return nil, fmt.Errorf("failed to find column info")
}

func getSelectColumnName(targetNode *ast.SelectColumnNode) (string, bool) {
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

type Schema struct {
	Name        string   `json:"name" yaml:"name"`
	Type        string   `json:"type" yaml:"type"`
	Mode        string   `json:"mode" yaml:"mode,omitempty"`
	Description string   `json:"description" yaml:"description,omitempty"`
	Fields      []Schema `json:"fields" yaml:"fields,omitempty"`
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

	var result []Schema
	err = json.Unmarshal(schemaJson, &result)
	if err != nil {
		fmt.Println(string(schemaJson))
		return nil, fmt.Errorf("failed to unmarshal json: %w", err)
	}
	schemaYaml, err := yaml.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal yaml: %w", err)
	}

	return []lsp.MarkedString{
		{
			Language: "markdown",
			Value:    resultStr,
		},
		{
			Language: "yaml",
			Value:    string(schemaYaml),
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

func byteOffsetToPosition(sql string, offset int) (lsp.Position, bool) {
	lines := strings.Split(sql, "\n")

	line := 0
	for _, l := range lines {
		if offset < len(l)+1 {
			return lsp.Position{
				Line:      line,
				Character: offset,
			}, true
		}

		line++
		offset -= len(l) + 1
	}

	return lsp.Position{}, false
}

type locationRangeNode interface {
	ParseLocationRange() *types.ParseLocationRange
}

func searchAstNode[T locationRangeNode](node ast.Node, termOffset int) (T, bool) {
	var targetNode T
	var found bool
	ast.Walk(node, func(n ast.Node) error {
		node, ok := n.(T)
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
			found = true
		}
		return nil
	})
	return targetNode, found
}

func searchResolvedAstNode[T locationRangeNode](output *zetasql.AnalyzerOutput, termOffset int) (T, bool) {
	var targetNode T
	var found bool
	rast.Walk(output.Statement(), func(n rast.Node) error {
		node, ok := n.(T)
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
			found = true
		}
		return nil
	})

	if found {
		return targetNode, found
	}
	return targetNode, false
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
