package pulse

import (
	"runtime"
	"runtime/debug"
	"strings"
)

const maxFrameworkVersionChars = 30

var frameworkModules = []struct{ path, name string }{
	{"github.com/gin-gonic/gin", "gin"},
	{"github.com/labstack/echo/v4", "echo"},
	{"github.com/go-chi/chi/v5", "chi"},
	{"github.com/gofiber/fiber/v2", "fiber"},
	{"github.com/gofiber/fiber/v3", "fiber"},
}

func currentFramework() (string, string) {
	info, _ := debug.ReadBuildInfo()
	return detectFramework(info, runtime.Version())
}

func moduleVersion(module *debug.Module) string {
	version := module.Version
	if module.Replace != nil && module.Replace.Version != "" {
		version = module.Replace.Version
	}
	return truncate(strings.TrimPrefix(version, "v"), maxFrameworkVersionChars)
}

func detectFramework(info *debug.BuildInfo, goVersion string) (string, string) {
	if info != nil {
		for _, known := range frameworkModules {
			for _, dependency := range info.Deps {
				if dependency.Path == known.path {
					return known.name, moduleVersion(dependency)
				}
			}
		}
	}
	return "go", truncate(strings.TrimPrefix(goVersion, "go"), maxFrameworkVersionChars)
}

func buildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return ""
	}
	return strings.TrimPrefix(info.Main.Version, "v")
}
