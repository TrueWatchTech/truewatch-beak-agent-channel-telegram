package main

import (
	"fmt"

	beak "github.com/TrueWatchTech/truewatch-beak-agent-channel-telegram"
)

func main() {
	connector := beak.NewConnector()
	fmt.Println(connector.Metadata().Label)
}
