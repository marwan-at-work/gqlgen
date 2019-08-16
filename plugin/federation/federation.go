package federation

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/99designs/gqlgen/codegen"
	"github.com/99designs/gqlgen/codegen/config"
	"github.com/99designs/gqlgen/codegen/templates"
	"github.com/99designs/gqlgen/plugin"
	"github.com/vektah/gqlparser"
	"github.com/vektah/gqlparser/ast"
	"github.com/vektah/gqlparser/formatter"
)

type federation struct {
	SDL      string
	Entities []*Entity
}

// New returns a federation plugin that injects
// federated directives and types into the schema
func New() plugin.Plugin {
	return &federation{}
}

func (f *federation) Name() string {
	return "federation"
}

func (f *federation) MutateConfig(cfg *config.Config) error {
	entityFields := map[string]config.TypeMapField{}
	for _, e := range f.Entities {
		entityFields[e.ResolverName] = config.TypeMapField{Resolver: true}
		for _, r := range e.Requires {
			if cfg.Models[e.Name].Fields == nil {
				model := cfg.Models[e.Name]
				model.Fields = map[string]config.TypeMapField{}
				cfg.Models[e.Name] = model
			}
			cfg.Models[e.Name].Fields[r.Name] = config.TypeMapField{Resolver: true}
		}
	}
	builtins := config.TypeMap{
		"_Service": {
			Model: config.StringList{
				"github.com/99designs/gqlgen/graphql/introspection.Service",
			},
		},
		"_Any": {Model: config.StringList{"github.com/99designs/gqlgen/graphql.Map"}},
		"Entity": {
			Fields: entityFields,
		},
	}
	for typeName, entry := range builtins {
		if cfg.Models.Exists(typeName) {
			return fmt.Errorf("%v already exists which must be reserved when Federation is enabled", typeName)
		}
		cfg.Models[typeName] = entry
	}
	cfg.Directives["external"] = config.DirectiveConfig{SkipRuntime: true}
	cfg.Directives["requires"] = config.DirectiveConfig{SkipRuntime: true}
	cfg.Directives["provides"] = config.DirectiveConfig{SkipRuntime: true}
	cfg.Directives["key"] = config.DirectiveConfig{SkipRuntime: true}
	cfg.Directives["extends"] = config.DirectiveConfig{SkipRuntime: true}

	return nil
}

func (f *federation) InjectSources(cfg *config.Config) {
	cfg.AdditionalSources = append(cfg.AdditionalSources, f.getSource(false))
	f.setEntities(cfg)
	s := "type Entity {\n"
	for _, e := range f.Entities {
		s += fmt.Sprintf("\t%s(%s: %s): %s!\n", e.ResolverName, e.FieldName, e.FieldTypeGQL, e.Name)
	}
	s += "}"
	cfg.AdditionalSources = append(cfg.AdditionalSources, &ast.Source{Name: "entity.graphql", Input: s, BuiltIn: true})
}

func (f *federation) MutateSchema(s *ast.Schema) error {
	// --- Set _Entity Union ---
	union := &ast.Definition{
		Name:        "_Entity",
		Kind:        ast.Union,
		Description: "A union unifies all @entity types (TODO: interfaces)",
		Types:       []string{},
	}
	for _, ent := range f.Entities {
		union.Types = append(union.Types, ent.Name)
		s.AddPossibleType("_Entity", ent.Def)
		// s.AddImplements(ent.Name, union) // Do we need this?
	}
	s.Types[union.Name] = union

	// --- Set _entities query ---
	fieldDef := &ast.FieldDefinition{
		Name: "_entities",
		Type: ast.NonNullListType(ast.NamedType("_Entity", nil), nil),
		Arguments: ast.ArgumentDefinitionList{
			{
				Name: "representations",
				Type: ast.NonNullListType(ast.NonNullNamedType("_Any", nil), nil),
			},
		},
	}
	s.Query.Fields = append(s.Query.Fields, fieldDef)

	// --- set _Service type ---
	typeDef := &ast.Definition{
		Kind: ast.Object,
		Name: "_Service",
		Fields: ast.FieldList{
			&ast.FieldDefinition{
				Name: "sdl",
				Type: ast.NonNullNamedType("String", nil),
			},
		},
	}
	s.Types[typeDef.Name] = typeDef

	// --- set _service query ---
	_serviceDef := &ast.FieldDefinition{
		Name: "_service",
		Type: ast.NonNullNamedType("_Service", nil),
	}
	s.Query.Fields = append(s.Query.Fields, _serviceDef)
	return nil
}

func (f *federation) getSource(builtin bool) *ast.Source {
	return &ast.Source{
		Name: "federation.graphql",
		Input: `# Declarations as required by the federation spec 
# See: https://www.apollographql.com/docs/apollo-server/federation/federation-spec/

scalar _Any
scalar _FieldSet

directive @external on FIELD_DEFINITION
directive @requires(fields: _FieldSet!) on FIELD_DEFINITION
directive @provides(fields: _FieldSet!) on FIELD_DEFINITION
directive @key(fields: _FieldSet!) on OBJECT | INTERFACE
directive @extends on OBJECT
`,
		BuiltIn: builtin,
	}
}

// Entity represents a federated type
// that was declared in the GQL schema.
type Entity struct {
	Name         string // The same name as the type declaration
	FieldName    string // The field name declared in @key
	FieldTypeGo  string // The Go representation of that field type
	FieldTypeGQL string // The GQL represetation of that field type
	ResolverName string // The resolver name, such as FindUserByID
	Def          *ast.Definition
	Requires     []*Requires
}

// Requires represents an @requires clause
type Requires struct {
	Name   string          // the name of the field
	Fields []*RequireField // the name of the sibling fields
}

// RequireField is similar to an entity but it is a field not
// an object
type RequireField struct {
	Name   string // The same name as the type declaration
	NameGo string // The Go struct field name
	TypeGo string // The Go representation of that field type
}

func (f *federation) GenerateCode(data *codegen.Data) error {
	sdl, err := f.getSDL(data.Config)
	if err != nil {
		return err
	}
	f.SDL = sdl
	data.Objects.ByName("Entity").Root = true
	for _, e := range f.Entities {
		obj := data.Objects.ByName(e.Name)
		for _, f := range obj.Fields {
			if f.Name == e.FieldName {
				e.FieldTypeGo = f.TypeReference.GO.String()
			}
			for _, r := range e.Requires {
				for _, rf := range r.Fields {
					if rf.Name == f.Name {
						rf.TypeGo = f.TypeReference.GO.String()
						rf.NameGo = f.GoFieldName
					}
				}
			}
		}
	}
	return templates.Render(templates.Options{
		Template:        tmpl,
		PackageName:     data.Config.Exec.Package,
		Filename:        "service.go",
		Data:            f,
		GeneratedHeader: true,
	})
}

func (f *federation) setEntities(cfg *config.Config) {
	schema, err := cfg.LoadSchema()
	if err != nil {
		panic(err)
	}
	for _, schemaType := range schema.Types {
		switch schemaType.Kind {
		case ast.Object:
			dir := schemaType.Directives.ForName("key") // TODO: interfaces
			if dir != nil {
				fieldName := dir.Arguments[0].Value.Raw // TODO: multiple arguments,a nd multiple keys
				if strings.Contains(fieldName, " ") {
					panic("only single fields are currently supported in @key declaration")
				}
				field := schemaType.Fields.ForName(fieldName)
				requires := []*Requires{}
				for _, f := range schemaType.Fields {
					dir := f.Directives.ForName("requires")
					if dir == nil {
						continue
					}
					fields := strings.Split(dir.Arguments[0].Value.Raw, " ")
					requireFields := []*RequireField{}
					for _, f := range fields {
						requireFields = append(requireFields, &RequireField{
							Name:   f,
							TypeGo: schemaType.Fields.ForName(f).Name,
						})
					}
					requires = append(requires, &Requires{
						Name:   f.Name,
						Fields: requireFields,
					})
				}
				f.Entities = append(f.Entities, &Entity{
					Name:         schemaType.Name,
					FieldName:    fieldName,
					FieldTypeGQL: field.Type.String(),
					Def:          schemaType,
					ResolverName: fmt.Sprintf("find%sBy%s", schemaType.Name, templates.ToGo(fieldName)),
					Requires:     requires,
				})
			}
		}
	}
}

func (f *federation) getSDL(c *config.Config) (string, error) {
	sources := []*ast.Source{f.getSource(true)}
	for _, filename := range c.SchemaFilename {
		filename = filepath.ToSlash(filename)
		var err error
		var schemaRaw []byte
		schemaRaw, err = ioutil.ReadFile(filename)
		if err != nil {
			fmt.Fprintln(os.Stderr, "unable to open schema: "+err.Error())
			os.Exit(1)
		}
		sources = append(sources, &ast.Source{Name: filename, Input: string(schemaRaw)})
	}
	schema, err := gqlparser.LoadSchema(sources...)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	formatter.NewFormatter(&buf).FormatSchema(schema)
	return buf.String(), nil
}

var tmpl = `
{{ reserveImport "context"  }}
{{ reserveImport "errors"  }}

{{ reserveImport "github.com/99designs/gqlgen/graphql/introspection" }}

func (ec *executionContext) __resolve__service(ctx context.Context) (introspection.Service, error) {
	if ec.DisableIntrospection {
		return introspection.Service{}, errors.New("federated introspection disabled")
	}
	return introspection.Service{
		SDL: ` + "`{{.SDL}}`" + `,
	}, nil
}

func (ec *executionContext) __resolve_entities(ctx context.Context, representations []map[string]interface{}) ([]_Entity, error) {
	list := []_Entity{}
	for _, rep := range representations {
		typeName, ok := rep["__typename"].(string)
		if !ok {
			return nil, errors.New("__typename must be an existing string")
		}
		switch typeName {
		{{ range .Entities }}
		case "{{.Name}}":
			id, ok := rep["{{.FieldName}}"].({{.FieldTypeGo}})
			if !ok {
				return nil, errors.New("opsies")
			}
			resp, err := ec.resolvers.Entity().{{.ResolverName | title}}(ctx, id)
			if err != nil {
				return nil, err
			}
			{{ range .Requires }}
				{{ range .Fields}}
					{{ .Name }}, _ := rep["{{.Name}}"].({{.TypeGo}})
					resp.{{.NameGo}} = {{.Name}}
				{{ end }}
			{{ end }}
			list = append(list, resp)
		{{ end }}
		default:
			return nil, errors.New("unknown type: "+typeName)
		}
	}
	return list, nil
}
`
