package jsLoader

import (
	"fmt"
	"path/filepath"
	"strings"
)

// "strings"

type LoaderError struct {
	err      error
	fileName string
}

func (le LoaderError) Error() string {
	return fmt.Sprintf("Error loading file %s:\n %s", le.fileName, le.err)
}

func LoadFile(src []byte, filePath string) ([]byte, []string, error) {
	tokens := lex(src)
	initialProgram, parseErr := parseTokens(tokens)
	if parseErr != nil {
		loaderErr := LoaderError{}
		loaderErr.err = parseErr
		loaderErr.fileName = filePath

		return nil, nil, loaderErr
	}

	resultProgram, fileImports := transformIntoModule(initialProgram, filePath)
	resultBytes := []byte(printAst(resultProgram))
	return resultBytes, fileImports, nil
}

type context struct {
	importedVars map[string]astNode
	parent       *context
}

func getImportedVariable(ctx *context, name astNode) astNode {
	if v, ok := ctx.importedVars[name.value]; ok {
		return v
	}
	return getImportedVariable(ctx.parent, name)
}

func transformIntoModule(src astNode, fileName string) (astNode, []string) {
	fileImports := []string{}

	var modifyAst,
		modifyProgram,
		modifyImport,
		modifyExport,
		modifyFunctionCall,
		modifyMemberExpression func(astNode, *context) astNode

	modifyAst = func(n astNode, ctx *context) astNode {
		switch n.t {

		case g_MEMBER_EXPRESSION:
			return modifyMemberExpression(n, ctx)

		case g_FUNCTION_CALL:
			return modifyFunctionCall(n, ctx)

		case g_EXPORT_STATEMENT:
			return modifyExport(n, ctx)

		case g_PROGRAM_STATEMENT:
			return modifyProgram(n, ctx)

		case g_IMPORT_STATEMENT:
			return modifyImport(n, ctx)

		case g_NAME:
			if importedVar, ok := ctx.importedVars[n.value]; ok {
				return importedVar
			}
			return n

		default:
			res := n
			res.children = []astNode{}
			for _, c := range n.children {
				res.children = append(res.children, modifyAst(c, ctx))
			}
			return res
		}
	}

	modifyMemberExpression = func(n astNode, ctx *context) astNode {
		children := []astNode{}
		for _, c := range n.children {
			children = append(children, modifyAst(c, ctx))
		}
		n.children = children

		if n.children[0].value == "module" &&
			n.children[1].value == "exports" {
			n.children[0].value = "exports"
			n.children[1].value = "default"
			return n
		}

		return n
	}

	modifyImport = func(n astNode, ctx *context) astNode {
		children := []astNode{}
		for _, c := range n.children {
			children = append(children, modifyAst(c, ctx))
		}
		n.children = children

		vars := n.children[0].children
		importAll := n.children[1].value
		importPath := n.children[2].value

		resolvedPath := resolveES6ImportPath(importPath, fileName)
		fileImports = append(fileImports, resolvedPath)

		ext := filepath.Ext(resolvedPath)

		objectName := CreateVarNameFromPath(resolvedPath)
		object := makeNode(g_NAME, objectName)

		if importAll != "" {
			alias := makeNode(g_NAME, importAll)
			ctx.importedVars[alias.value] = object
		}

		for _, v := range vars {
			alias := v.children[1]

			if ext == ".js" {
				property := v.children[0]

				moduleName := makeNode(g_NAME, objectName)
				modulesObj := makeNode(g_NAME, "modules")
				moduleMember := makeNode(g_MEMBER_EXPRESSION, "", modulesObj, moduleName)

				member := makeNode(g_MEMBER_EXPRESSION, "", moduleMember, property)

				ctx.importedVars[alias.value] = member
			} else {
				filePath := "'" + objectName + ext + "'"
				fileURL := makeNode(g_STRING_LITERAL, filePath)

				ctx.importedVars[alias.value] = fileURL
			}

		}

		return makeNode(g_EMPTY_EXPRESSION, "")
	}

	modifyFunctionCall = func(n astNode, ctx *context) astNode {
		children := []astNode{}
		for _, c := range n.children {
			children = append(children, modifyAst(c, ctx))
		}
		n.children = children

		nameNode := children[0]
		args := children[1].children

		if nameNode.value == "require" {
			if len(args) == 1 && args[0].t == g_STRING_LITERAL {
				path := args[0].value
				resolvedPath := resolveES6ImportPath(path, fileName)
				fileImports = append(fileImports, resolvedPath)

				objectName := CreateVarNameFromPath(resolvedPath)

				moduleName := makeNode(g_NAME, objectName)
				modulesObj := makeNode(g_NAME, "modules")
				moduleMember := makeNode(g_MEMBER_EXPRESSION, "", modulesObj, moduleName)

				defaultName := makeNode(g_NAME, "default")
				moduleDefaultExport := makeNode(g_MEMBER_EXPRESSION, "", moduleMember, defaultName)

				return moduleDefaultExport
			}

			fmt.Printf("Wrong arguments to require function")
			return n
		}

		return n
	}

	modifyProgram = func(n astNode, ctx *context) astNode {
		children := []astNode{}
		for _, c := range n.children {
			children = append(children, modifyAst(c, ctx))
		}
		n.children = children

		statements := []astNode{}

		// add var exports = {}
		exportsObj := makeNode(g_NAME, "exports")
		{
			right := makeNode(g_OBJECT_LITERAL, "")
			decl := makeNode(g_DECLARATOR, "", exportsObj, right)
			declExpr := makeNode(g_DECLARATION_EXPRESSION, "var", decl)
			declSt := makeNode(g_DECLARATION_STATEMENT, "", declExpr)
			statements = append(statements, declSt)
		}

		// add all other statements
		for _, st := range n.children {
			statements = append(statements, modifyAst(st, ctx))
		}

		// add return exports
		ret := makeNode(g_RETURN_STATEMENT, "", exportsObj)
		statements = append(statements, ret)

		params := makeNode(g_FUNCTION_PARAMETERS, "")
		blockSt := makeNode(g_BLOCK_STATEMENT, "", statements...)
		funcExpr := makeNode(g_FUNCTION_EXPRESSION, "", params, blockSt)

		{
			moduleFnsArray := makeNode(g_NAME, "moduleFns")

			moduleName := CreateVarNameFromPath(fileName)
			prop := makeNode(g_NAME, moduleName)
			memExpr := makeNode(g_MEMBER_EXPRESSION, "", moduleFnsArray, prop)

			assignmentExpr := makeNode(g_EXPRESSION, "=", memExpr, funcExpr)
			assignmentSt := makeNode(g_EXPRESSION_STATEMENT, "", assignmentExpr)

			return assignmentSt
		}
	}

	modifyExport = func(n astNode, ctx *context) astNode {
		children := []astNode{}
		for _, c := range n.children {
			children = append(children, modifyAst(c, ctx))
		}
		n.children = children

		vars := n.children[0].children
		exportsObj := makeNode(g_NAME, "exports")

		var member astNode
		pathNode := n.children[2]
		if pathNode.value != "" {
			resolvedPath := resolveES6ImportPath(pathNode.value, fileName)
			fileImports = append(fileImports, resolvedPath)
			objectName := CreateVarNameFromPath(resolvedPath)
			importObj := makeNode(g_NAME, objectName)

			modulesObj := makeNode(g_NAME, "modules")
			member = makeNode(g_MEMBER_EXPRESSION, "", modulesObj, importObj)
		}

		if !(n.flags&f_EXPORT_ALL != 0) {
			assignments := []astNode{}
			for _, v := range vars {
				exportedName := v.children[1]
				left := makeNode(g_MEMBER_EXPRESSION, "", exportsObj, exportedName)
				var right astNode

				if pathNode.value != "" {
					property := v.children[0]
					right = makeNode(g_MEMBER_EXPRESSION, "", member, property)
				} else {
					right = v.children[0]
				}

				d := makeNode(g_EXPRESSION, "=", left, right)
				assignments = append(assignments, d)
			}
			seqExpr := makeNode(g_SEQUENCE_EXPRESSION, "=", assignments...)
			exprSt := makeNode(g_EXPRESSION_STATEMENT, "", seqExpr)

			decl := n.children[1]

			multiSt := makeNode(g_MULTISTATEMENT, "", decl, exprSt)

			return multiSt
		}

		obj := makeNode(g_NAME, "Object")
		assign := makeNode(g_NAME, "assign")
		funcName := makeNode(g_MEMBER_EXPRESSION, "", obj, assign)

		args := []astNode{
			exportsObj,
			member,
		}
		argsNode := makeNode(g_FUNCTION_ARGS, "", args...)
		objectAssignCall := makeNode(g_FUNCTION_CALL, "", funcName, argsNode)

		exprSt := makeNode(g_EXPRESSION_STATEMENT, "", objectAssignCall)
		return exprSt
	}

	ctx := context{}
	ctx.importedVars = make(map[string]astNode)
	res := modifyAst(src, &ctx)

	return res, fileImports
}

func CreateVarNameFromPath(path string) string {
	newName := strings.Replace(path, "/", "_", -1)
	newName = strings.Replace(newName, ".", "_", -1)
	newName = strings.Replace(newName, "-", "_", -1)
	return newName
}

func makeToken(text string) token {
	res := lex([]byte(text))
	return res[0]
}

func resolveES6ImportPath(importPath, currentFileName string) string {
	importPath = trimQuotesFromString(importPath)
	pathParts := strings.Split(importPath, "/")

	locationParts := strings.Split(currentFileName, "/")
	locationParts = locationParts[:len(locationParts)-1]

	// import from node_modules
	if len(pathParts) > 0 {
		if pathParts[0] != "." && pathParts[0] != ".." {
			locationParts = []string{"node_modules"}
			if len(pathParts) == 1 {
				pathParts = append(pathParts, "index.js")
			}
		}
	}

	for _, part := range pathParts {
		if part == ".." {
			locationParts = locationParts[:len(locationParts)-1]
			pathParts = pathParts[1:]
		}
		if part == "." {
			pathParts = pathParts[1:]
		}
	}

	fullFileName := strings.Join(append(locationParts, pathParts...), "/")

	ext := ""
	if strings.Index(pathParts[len(pathParts)-1], ".") < 0 {
		ext = ".js"
	}

	result := fullFileName + ext
	return result
}
