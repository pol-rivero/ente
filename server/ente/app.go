package ente

type App string

const (
	Photos App = "photos"
	Auth   App = "auth"
	Locker App = "locker"
	Drive  App = "drive"
)

func (a App) IsValid() bool {
	switch a {
	case Photos, Auth, Locker, Drive:
		return true
	}
	return false
}

func (a App) IsValidForCollection() bool {
	switch a {
	case Photos, Locker, Drive:
		return true
	}
	return false
}

func IsCrossAppWithDrive(a, b App) bool {
	return (a == Drive || b == Drive) && a != b
}
