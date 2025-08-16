package logging

import (
	"log"
	"os"
)

var Logger *log.Logger

func Init(logFile string) error {
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	Logger = log.New(f, "", log.LstdFlags|log.Lmsgprefix)
	return nil
}
