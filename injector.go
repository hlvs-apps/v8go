package v8go

// Injector is an interface that allows for injecting a custom
// implementation to the v8go isolate.
type Injector interface {
	// Inject is called when the isolate is created and allows
	// for injecting a custom implementation to the isolate.
	Inject(*Isolate, *ObjectTemplate) error
}
