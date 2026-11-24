package core

const DefaultEnvironment = "default"

func AppTarget(app string) Target {
	return Target{App: app, Environment: DefaultEnvironment}
}
