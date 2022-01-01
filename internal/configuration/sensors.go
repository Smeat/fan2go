package configuration

type SensorConfig struct {
	ID    string             `json:"id"`
	HwMon *HwMonSensorConfig `json:"hwMon,omitempty"`
	File  *FileSensorConfig  `json:"file,omitempty"`
	Command  *CommandSensorConfig  `json:"command,omitempty"`
}

type HwMonSensorConfig struct {
	Platform  string `json:"platform"`
	Index     int    `json:"index"`
	TempInput string
}

type FileSensorConfig struct {
	Path string `json:"path"`
}

type CommandSensorConfig struct {
	Cmd string `json:"cmd"`
}
