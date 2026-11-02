package config

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

type transferShapeKey struct {
	node   *yaml.Node
	target reflect.Type
}

type transferShapeWalker struct {
	active    map[transferShapeKey]bool
	seen      map[transferShapeKey]bool
	remaining int
}

// transferNodeShape 补足 YAML 的隐式标量转换，不把私有原值写进错误。
// 同一锚点只检查一次，并限制总工作量，避免别名图导致指数级遍历。
func transferNodeShape(node *yaml.Node, target reflect.Type, field string, depth int) error {
	walker := transferShapeWalker{active: make(map[transferShapeKey]bool), seen: make(map[transferShapeKey]bool), remaining: 65536}
	return walker.check(node, target, field, depth)
}

func (walker *transferShapeWalker) check(node *yaml.Node, target reflect.Type, field string, depth int) error {
	invalid := func() error {
		line := 0
		if node != nil {
			line = node.Line
		}
		return &TransferError{Code: "invalid_import", Message: "配置字段类型、嵌套或结构不合法，请检查对应位置", Field: field, Line: line}
	}
	if node == nil || depth > 100 || walker.remaining <= 0 {
		return invalid()
	}
	walker.remaining--
	key := transferShapeKey{node: node, target: target}
	if walker.active[key] {
		return invalid()
	}
	if walker.seen[key] {
		return nil
	}
	walker.active[key] = true
	defer func() { delete(walker.active, key); walker.seen[key] = true }()
	if node.Kind == yaml.AliasNode {
		return walker.check(node.Alias, target, field, depth+1)
	}
	if target != nil && node.Tag == "!!null" {
		if target.Kind() == reflect.Pointer || target.Kind() == reflect.Map || target.Kind() == reflect.Slice || target.Kind() == reflect.Interface {
			return nil
		}
		return invalid()
	}
	if target == nil {
		return walker.checkJSON(node, field, depth)
	}
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	switch target.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return invalid()
		}
		fields := make(map[string]reflect.StructField)
		for index := 0; index < target.NumField(); index++ {
			item := target.Field(index)
			name := strings.Split(item.Tag.Get("yaml"), ",")[0]
			if name != "" && name != "-" {
				fields[name] = item
			}
		}
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key.Tag == "!!merge" {
				if err := walker.checkMerge(value, target, field, depth+1); err != nil {
					return err
				}
				continue
			}
			item, ok := fields[key.Value]
			if !ok || key.Tag != "!!str" {
				return invalid()
			}
			path := key.Value
			if field != "" {
				path = field + "." + path
			}
			if err := walker.check(value, item.Type, path, depth+1); err != nil {
				return err
			}
		}
	case reflect.Map:
		if node.Kind != yaml.MappingNode {
			return invalid()
		}
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key.Tag == "!!merge" {
				if err := walker.checkMerge(value, target, field, depth+1); err != nil {
					return err
				}
				continue
			}
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return invalid()
			}
			path := field
			// 提供商名称是选择身份；私有请求头／请求体的键名不回显。
			if field == "providers" {
				path += "." + key.Value
			}
			if err := walker.check(value, target.Elem(), path, depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if node.Kind != yaml.SequenceNode {
			return invalid()
		}
		for index, value := range node.Content {
			if err := walker.check(value, target.Elem(), fmt.Sprintf("%s[%d]", field, index), depth+1); err != nil {
				return err
			}
		}
	case reflect.String:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
			return invalid()
		}
	case reflect.Bool:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" {
			return invalid()
		}
	case reflect.Int, reflect.Int64:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
			return invalid()
		}
	case reflect.Interface:
		return walker.checkJSON(node, field, depth)
	default:
		return invalid()
	}
	return nil
}

func (walker *transferShapeWalker) checkMerge(node *yaml.Node, target reflect.Type, field string, depth int) error {
	if node.Kind == yaml.SequenceNode {
		for _, child := range node.Content {
			if err := walker.check(child, target, field, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return walker.check(node, target, field, depth+1)
}

func (walker *transferShapeWalker) checkJSON(node *yaml.Node, field string, depth int) error {
	invalid := func() error {
		return &TransferError{Code: "invalid_import", Message: "自定义请求体只允许 JSON 数据类型", Field: field, Line: node.Line}
	}
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key.Tag == "!!merge" {
				if err := walker.checkMerge(value, nil, field, depth+1); err != nil {
					return err
				}
				continue
			}
			if key.Tag != "!!str" {
				return invalid()
			}
			if err := walker.check(value, nil, field, depth+1); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			if err := walker.check(child, nil, field, depth+1); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!str", "!!null", "!!int", "!!float", "!!bool":
			return nil
		default:
			return invalid()
		}
	default:
		return invalid()
	}
	return nil
}
