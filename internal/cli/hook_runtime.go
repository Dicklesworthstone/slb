package cli

import _ "embed"

// hookRuntime is embedded so installed hooks remain standalone, while their
// protocol and offline audit implementation can be tested directly with Python.
//
//go:embed hook_runtime.py
var hookRuntime string
