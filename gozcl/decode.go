package gozcl

import (
	"fmt"
	"reflect"

	"github.com/apparentlymart/go-cty/cty/convert"
	"github.com/apparentlymart/go-cty/cty/gocty"
	"github.com/zclconf/go-zcl/zcl"
)

// DecodeBody extracts the configuration within the given body into the given
// value. This value must be a non-nil pointer to either a struct or
// a map, where in the former case the configuration will be decoded using
// struct tags and in the latter case only attributes are allowed and their
// values are decoded into the map.
//
// The given EvalContext is used to resolve any variables or functions in
// expressions encountered while decoding. This may be nil to require only
// constant values, for simple applications that do not support variables or
// functions.
//
// The returned diagnostics should be inspected with its HasErrors method to
// determine if the populated value is valid and complete. If error diagnostics
// are returned then the given value may have been partially-populated but
// may still be accessed by a careful caller for static analysis and editor
// integration use-cases.
func DecodeBody(body zcl.Body, ctx *zcl.EvalContext, val interface{}) zcl.Diagnostics {
	rv := reflect.ValueOf(val)
	if rv.Kind() != reflect.Ptr {
		panic(fmt.Sprintf("target value must be a pointer, not %s", rv.Type().String()))
	}

	return decodeBodyToValue(body, ctx, rv.Elem())
}

func decodeBodyToValue(body zcl.Body, ctx *zcl.EvalContext, val reflect.Value) zcl.Diagnostics {
	et := val.Type()
	switch et.Kind() {
	case reflect.Struct:
		return decodeBodyToStruct(body, ctx, val)
	case reflect.Map:
		return decodeBodyToMap(body, ctx, val)
	default:
		panic(fmt.Sprintf("target value must be pointer to struct or map, not %s", et.String()))
	}
}

func decodeBodyToStruct(body zcl.Body, ctx *zcl.EvalContext, val reflect.Value) zcl.Diagnostics {
	schema, partial := ImpliedBodySchema(val.Interface())

	var content *zcl.BodyContent
	var leftovers zcl.Body
	var diags zcl.Diagnostics
	if partial {
		content, leftovers, diags = body.PartialContent(schema)
	} else {
		content, diags = body.Content(schema)
	}
	if content == nil {
		return diags
	}

	tags := getFieldTags(val.Type())

	if tags.Remain != nil {
		fieldIdx := *tags.Remain
		field := val.Type().Field(fieldIdx)
		fieldV := val.Field(fieldIdx)
		switch {
		case bodyType.AssignableTo(field.Type):
			fieldV.Set(reflect.ValueOf(leftovers))
		case attrsType.AssignableTo(field.Type):
			attrs, attrsDiags := leftovers.JustAttributes()
			if len(attrsDiags) > 0 {
				diags = append(diags, attrsDiags...)
			}
			fieldV.Set(reflect.ValueOf(attrs))
		default:
			diags = append(diags, decodeBodyToValue(leftovers, ctx, fieldV)...)
		}
	}

	for name, attr := range content.Attributes {
		fieldIdx := tags.Attributes[name]
		field := val.Type().Field(fieldIdx)
		fieldV := val.Field(fieldIdx)

		switch {
		case attrType.AssignableTo(field.Type):
			fieldV.Set(reflect.ValueOf(attr))
		case exprType.AssignableTo(field.Type):
			fieldV.Set(reflect.ValueOf(attr.Expr))
		default:
			diags = append(diags, DecodeExpression(
				attr.Expr, ctx, fieldV.Addr().Interface(),
			)...)
		}
	}

	blocksByType := content.Blocks.ByType()

	for typeName, fieldIdx := range tags.Blocks {
		blocks := blocksByType[typeName]
		field := val.Type().Field(fieldIdx)

		ty := field.Type
		isSlice := false
		isPtr := false
		if ty.Kind() == reflect.Slice {
			isSlice = true
			ty = ty.Elem()
		}
		if ty.Kind() == reflect.Ptr {
			isPtr = true
			ty = ty.Elem()
		}

		if len(blocks) > 1 && !isSlice {
			diags = append(diags, &zcl.Diagnostic{
				Severity: zcl.DiagError,
				Summary:  fmt.Sprintf("Duplicate %s block", typeName),
				Detail: fmt.Sprintf(
					"Only one %s block is allowed. Another was defined at %s.",
					typeName, blocks[0].DefRange.String(),
				),
				Subject: &blocks[1].DefRange,
			})
			continue
		}

		if len(blocks) == 0 {
			if isSlice || isPtr {
				val.Field(fieldIdx).Set(reflect.Zero(field.Type))
			} else {
				diags = append(diags, &zcl.Diagnostic{
					Severity: zcl.DiagError,
					Summary:  fmt.Sprintf("Missing %s block", typeName),
					Detail:   fmt.Sprintf("A %s block is required.", typeName),
					Subject:  body.MissingItemRange().Ptr(),
				})
			}
			continue
		}

		switch {

		case isSlice:
			elemType := ty
			if isPtr {
				elemType = reflect.PtrTo(ty)
			}
			sli := reflect.MakeSlice(reflect.SliceOf(elemType), len(blocks), len(blocks))

			for i, block := range blocks {
				if isPtr {
					v := reflect.New(ty)
					diags = append(diags, decodeBlockToValue(block, ctx, v.Elem())...)
					sli.Index(i).Set(v)
				} else {
					diags = append(diags, decodeBlockToValue(block, ctx, sli.Index(i))...)
				}
			}

			val.Field(fieldIdx).Set(sli)

		default:
			block := blocks[0]
			if isPtr {
				v := reflect.New(ty)
				diags = append(diags, decodeBlockToValue(block, ctx, v.Elem())...)
				val.Field(fieldIdx).Set(v)
			} else {
				diags = append(diags, decodeBlockToValue(block, ctx, val.Field(fieldIdx))...)
			}

		}

	}

	return diags
}

func decodeBodyToMap(body zcl.Body, ctx *zcl.EvalContext, v reflect.Value) zcl.Diagnostics {
	attrs, diags := body.JustAttributes()
	if attrs == nil {
		return diags
	}

	mv := reflect.MakeMap(v.Type())

	for k, attr := range attrs {
		switch {
		case attrType.AssignableTo(v.Type().Elem()):
			mv.SetMapIndex(reflect.ValueOf(k), reflect.ValueOf(attr))
		case exprType.AssignableTo(v.Type().Elem()):
			mv.SetMapIndex(reflect.ValueOf(k), reflect.ValueOf(attr.Expr))
		default:
			ev := reflect.New(v.Type().Elem())
			diags = append(diags, DecodeExpression(attr.Expr, ctx, ev.Interface())...)
			mv.SetMapIndex(reflect.ValueOf(k), ev.Elem())
		}
	}

	v.Set(mv)

	return diags
}

func decodeBlockToValue(block *zcl.Block, ctx *zcl.EvalContext, v reflect.Value) zcl.Diagnostics {
	var diags zcl.Diagnostics

	ty := v.Type()

	switch {
	case blockType.AssignableTo(ty):
		v.Elem().Set(reflect.ValueOf(block))
	case bodyType.AssignableTo(ty):
		v.Elem().Set(reflect.ValueOf(block.Body))
	case attrsType.AssignableTo(ty):
		attrs, attrsDiags := block.Body.JustAttributes()
		if len(attrsDiags) > 0 {
			diags = append(diags, attrsDiags...)
		}
		v.Elem().Set(reflect.ValueOf(attrs))
	default:
		diags = append(diags, decodeBodyToValue(block.Body, ctx, v)...)

		if len(block.Labels) > 0 {
			blockTags := getFieldTags(ty)
			for li, lv := range block.Labels {
				lfieldIdx := blockTags.Labels[li].FieldIndex
				v.Field(lfieldIdx).Set(reflect.ValueOf(lv))
			}
		}

	}

	return diags
}

// DecodeExpression extracts the value of the given expression into the given
// value. This value must be something that gocty is able to decode into,
// since the final decoding is delegated to that package.
//
// The given EvalContext is used to resolve any variables or functions in
// expressions encountered while decoding. This may be nil to require only
// constant values, for simple applications that do not support variables or
// functions.
//
// The returned diagnostics should be inspected with its HasErrors method to
// determine if the populated value is valid and complete. If error diagnostics
// are returned then the given value may have been partially-populated but
// may still be accessed by a careful caller for static analysis and editor
// integration use-cases.
func DecodeExpression(expr zcl.Expression, ctx *zcl.EvalContext, val interface{}) zcl.Diagnostics {
	srcVal, diags := expr.Value(ctx)

	convTy, err := gocty.ImpliedType(val)
	if err != nil {
		panic(fmt.Sprintf("unsuitable DecodeExpression target: %s", err))
	}

	srcVal, err = convert.Convert(srcVal, convTy)
	if err != nil {
		diags = append(diags, &zcl.Diagnostic{
			Severity: zcl.DiagError,
			Summary:  "Unsuitable value type",
			Detail:   fmt.Sprintf("Incorrect value type: %s", err.Error()),
			Subject:  expr.StartRange().Ptr(),
			Context:  expr.Range().Ptr(),
		})
		return diags
	}

	err = gocty.FromCtyValue(srcVal, val)
	if err != nil {
		diags = append(diags, &zcl.Diagnostic{
			Severity: zcl.DiagError,
			Summary:  "Unsuitable value type",
			Detail:   fmt.Sprintf("Incorrect value type: %s", err.Error()),
			Subject:  expr.StartRange().Ptr(),
			Context:  expr.Range().Ptr(),
		})
	}

	return diags
}
