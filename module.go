package wazergo

import (
	"context"

	. "github.com/stealthrocket/wazergo/types"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Module is a type constraint used to validate that all module instances
// created from wazero host modules abide to the same set of requirements.
type Module interface{ api.Closer }

// HostModule is an interface representing type-safe wazero host modules.
// The interface is parametrized on the module type that it instantiates.
//
// HostModule instances are expected to be immutable and therfore safe to use
// concurrently from multiple goroutines.
type HostModule[T Module] interface {
	// Returns the name of the host module (e.g. "wasi_snapshot_preview1").
	Name() string
	// Returns the collection of functions exported by the host module.
	// The method may return the same value across multiple calls to this
	// method, the program is expected to treat it as a read-only value.
	Functions() Functions[T]
	// Creates a new instance of the host module type, using the list of options
	// passed as arguments to configure it. This method is intended to be called
	// automatically when instantiating a module via an instantiation context.
	Instantiate(ctx context.Context, options ...Option[T]) (T, error)
}

// Build builds the host module p in the wazero runtime r, returning the
// instance of HostModuleBuilder that was created. This is a low level function
// which is only exposed for certain advanced use cases where a program might
// not be able to leverage Compile/Instantiate, most application should not need
// to use this function.
func Build[T Module](runtime wazero.Runtime, mod HostModule[T]) wazero.HostModuleBuilder {
	moduleName := mod.Name()
	builder := runtime.NewHostModuleBuilder(moduleName)

	for export, fn := range mod.Functions() {
		if fn.Name == "" {
			fn.Name = export
		}

		paramTypes := appendValueTypes(make([]api.ValueType, 0, fn.StackParamCount()), fn.Params)
		resultTypes := appendValueTypes(make([]api.ValueType, 0, fn.StackResultCount()), fn.Results)

		builder.NewFunctionBuilder().
			WithGoModuleFunction(bind(fn.Func), paramTypes, resultTypes).
			WithName(fn.Name).
			Export(export)
	}

	return builder
}

func appendValueTypes(buffer []api.ValueType, values []Value) []api.ValueType {
	for _, v := range values {
		buffer = append(buffer, v.ValueTypes()...)
	}
	return buffer
}

func bind[T Module](f func(T, context.Context, api.Module, []uint64)) api.GoModuleFunction {
	return contextualizedGoModuleFunction[T](f)
}

type contextualizedGoModuleFunction[T Module] func(T, context.Context, api.Module, []uint64)

func (f contextualizedGoModuleFunction[T]) Call(ctx context.Context, module api.Module, stack []uint64) {
	this := ctx.Value((*ModuleInstance[T])(nil)).(T)
	f(this, ctx, module, stack)
}

// CompiledModule represents a compiled version of a wazero host module.
type CompiledModule[T Module] struct {
	HostModule HostModule[T]
	wazero.CompiledModule
	// The compiled module captures the runtime that it was compiled for since
	// instantiation of the host module must happen in the same runtime.
	// This prevents application from having to pass the runtime again when
	// instantiating the module, which is redundant and sometimes error prone
	// (e.g. the wrong runtime could be used during instantiation).
	runtime wazero.Runtime
}

// Compile compiles a wazero host module within the given context.
func Compile[T Module](ctx context.Context, runtime wazero.Runtime, mod HostModule[T]) (*CompiledModule[T], error) {
	compiledModule, err := Build(runtime, mod).Compile(ctx)
	if err != nil {
		return nil, err
	}
	return &CompiledModule[T]{mod, compiledModule, runtime}, nil
}

// MustCompile is like Compile but it panics if there is an error.
func MustCompile[T Module](ctx context.Context, runtime wazero.Runtime, mod HostModule[T]) *CompiledModule[T] {
	compiledModule, err := Compile(ctx, runtime, mod)
	if err != nil {
		panic(err)
	}
	return compiledModule
}

// Instantiate creates an instance of the compiled module for in the given runtime
//
// Instantiate may be called multiple times to create multiple copies of the host
// module state. This is useful to allow the program to create scopes where the
// state of the host module needs to bind uniquely to a subset of the guest
// modules instantiated in the runtime.
func (c *CompiledModule[T]) Instantiate(ctx context.Context, options ...Option[T]) (*ModuleInstance[T], error) {
	// TODO: relying on the name here may be inaccurate, we are making the
	// assumption that the program did not register a different module under
	// the same name.
	moduleName := c.HostModule.Name()
	module := c.runtime.Module(moduleName)
	if module == nil {
		config := wazero.NewModuleConfig().WithStartFunctions()
		m, err := c.runtime.InstantiateModule(ctx, c.CompiledModule, config)
		if err != nil {
			return nil, err
		}
		module = m
	}
	instance, err := c.HostModule.Instantiate(ctx, options...)
	if err != nil {
		return nil, err
	}
	return &ModuleInstance[T]{module, moduleName, instance}, nil
}

// ModuleInstance represents a module instance created from a compiled host module.
type ModuleInstance[T Module] struct {
	api.Module
	moduleName string
	instance   T
}

func (m *ModuleInstance[T]) String() string {
	return "module[" + m.moduleName + "]"
}

func (m *ModuleInstance[T]) Name() string {
	return m.moduleName
}

func (m *ModuleInstance[T]) ExportedFunction(name string) api.Function {
	if f := m.Module.ExportedFunction(name); f != nil {
		return &moduleInstanceFunction[T]{f, m}
	}
	return nil
}

func (m *ModuleInstance[T]) Close(ctx context.Context) error {
	return m.instance.Close(ctx)
}

func (m *ModuleInstance[T]) CloseWithExitCode(ctx context.Context, _ uint32) error {
	return m.Close(ctx)
}

type moduleInstanceFunction[T Module] struct {
	api.Function
	instance *ModuleInstance[T]
}

func (f *moduleInstanceFunction[T]) Call(ctx context.Context, params ...uint64) ([]uint64, error) {
	return f.Function.Call(WithModuleInstance(ctx, f.instance), params...)
}

func (f *moduleInstanceFunction[T]) CallWithStack(ctx context.Context, stack []uint64) error {
	return f.Function.CallWithStack(WithModuleInstance(ctx, f.instance), stack)
}

// Instantiate compiles and instantiates a host module.
func Instantiate[T Module](ctx context.Context, runtime wazero.Runtime, mod HostModule[T], options ...Option[T]) (*ModuleInstance[T], error) {
	c, err := Compile[T](ctx, runtime, mod)
	if err != nil {
		return nil, err
	}
	return c.Instantiate(ctx, options...)
}

// MustInstantiate is like Instantiate but it panics if an error is encountered.
func MustInstantiate[T Module](ctx context.Context, runtime wazero.Runtime, mod HostModule[T], options ...Option[T]) *ModuleInstance[T] {
	instance, err := Instantiate(ctx, runtime, mod, options...)
	if err != nil {
		panic(err)
	}
	return instance
}

// WithModuleInstance returns a Go context inheriting from ctx and containing
// the state needed for module instantiated from wazero host module to properly
// bind their methods to their receiver (e.g. the module instance).
//
// Use this function when calling methods of an instantiated WebAssenbly module
// which may invoke exported functions of a wazero host module, for example:
//
//	// The program first creates the modules instances for the host modules.
//	instance1 := wazergo.MustInstantiate(ctx, runtime, firstHostModule)
//	instance2 := wazergo.MustInstantiate(ctx, runtime, otherHostModule)
//
//	...
//
//	// In this example the parent is the background context, but it might be any
//	// other Go context relevant to the application.
//	ctx := context.Background()
//	ctx = wazergo.WithModuleInstance(ctx, instance1)
//	ctx = wazergo.WithModuleInstance(ctx, instance2)
//
//	start := module.ExportedFunction("_start")
//	r, err := start.Call(ctx)
//	if err != nil {
//		...
//	}
func WithModuleInstance[T Module](ctx context.Context, ins *ModuleInstance[T]) context.Context {
	return context.WithValue(ctx, (*ModuleInstance[T])(nil), ins.instance)
}
