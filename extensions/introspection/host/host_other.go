//go:build !linux && !darwin

package host

func loadAvg() ([3]float64, error) { return [3]float64{}, ErrUnsupported }

func memory() (total, avail uint64, err error) { return 0, 0, ErrUnsupported }
