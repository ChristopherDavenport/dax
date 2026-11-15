package agent

// The extensions' after-tool hooks (extension.Extension.AfterToolCall):
// agentkit's WithAfterToolCall keeps the last hook set, so the session
// composes every extension's into one, in the extensions' order, and
// gives it to the main agent and, through Env.ChildAfterToolCall, to
// every sub-agent (#187).

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"strings"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentturn"

	"github.com/ChristopherDavenport/dax/extension"
)

// afterHook is an after-tool hook.
type afterHook = func(context.Context, agentturn.ToolResultInfo) (*agentturn.ToolOverride, error)

// afterToolHooks builds the extensions' AfterToolCall hooks over env,
// in the extensions' order, and the names of the extensions that gave
// one.
func afterToolHooks(exts []extension.Extension, env extension.Env) ([]afterHook, []string, error) {
	var hooks []afterHook
	var names []string
	for _, e := range exts {
		if e.AfterToolCall == nil {
			continue
		}
		h, err := e.AfterToolCall(env)
		if err != nil {
			return nil, nil, fmt.Errorf("extension %s: after tool call: %w", e.Name, err)
		}
		if h != nil {
			hooks, names = append(hooks, h), append(names, e.Name)
		}
	}
	return hooks, names, nil
}

// chainAfter runs hs in order, each seeing the result the one before it
// left, and returns the last replacement any made; nil for none. A
// hook's error ends the chain with it.
func chainAfter(hs []afterHook) afterHook {
	if len(hs) == 0 {
		return nil
	}
	return func(ctx context.Context, info agentturn.ToolResultInfo) (*agentturn.ToolOverride, error) {
		var out *agentturn.ToolOverride
		for _, h := range hs {
			ov, err := h(ctx, info)
			if err != nil {
				return out, err
			}
			if ov != nil {
				out = ov
				info.Result, info.Err = ov.Result, ov.Err
			}
		}
		return out, nil
	}
}

// kitSetsAfter refuses an extension's Kit options when one of them is
// agentkit.WithAfterToolCall, which would replace the hook the session
// gives the kit, every extension's AfterToolCall with it. An option is
// a function, so it is known by its name: WithAfterToolCall's closure,
// "agentkit.WithAfterToolCall.func1", or where the compiler inlined
// it, the caller's name with ".WithAfterToolCall." and a number.
func kitSetsAfter(name string, opts []agentkit.Option, withHooks []string) error {
	for _, o := range opts {
		if f := runtime.FuncForPC(funcPC(o)); f != nil && strings.Contains(f.Name(), ".WithAfterToolCall.") {
			return afterReplaced("extension "+name, withHooks)
		}
	}
	return nil
}

// checkKitAfter refuses a kit whose after-tool hook is not the
// session's composition, set some way kitSetsAfter did not see.
func checkKitAfter(kit *agentkit.Kit, ours afterHook, withHooks []string) error {
	got := kit.Config().AfterToolCall
	if got == nil || ours != nil && funcPC(got) == funcPC(ours) {
		return nil
	}
	return afterReplaced("an extension", withHooks)
}

// afterReplaced is the error that who set agentkit.WithAfterToolCall,
// naming the first extension whose hook it would replace.
func afterReplaced(who string, withHooks []string) error {
	replaced := "the session's after-tool hooks"
	if len(withHooks) > 0 {
		replaced = "extension " + withHooks[0] + "'s AfterToolCall hook"
	}
	return fmt.Errorf("%s sets agentkit.WithAfterToolCall through its Kit options, which would replace %s: agentkit keeps one such hook, so dax composes them; give it an AfterToolCall hook (extension.Extension.AfterToolCall) instead", who, replaced)
}

// funcPC is the entry of fn's code, which tells two hooks apart.
func funcPC(fn any) uintptr { return reflect.ValueOf(fn).Pointer() }

func (e *sessionEnv) ChildAfterToolCall() func(context.Context, agentturn.ToolResultInfo) (*agentturn.ToolOverride, error) {
	return e.s.afterTool
}
