//go:build !android && !linux && !darwin

package mobile

func prepareEnergyProfileSignals() func() { return func() {} }
