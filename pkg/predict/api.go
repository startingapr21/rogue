package predict

import "github.com/startingapr21/rogue/pkg/config"

type HelpResponse struct {
	Arguments map[string]*config.RunArgument `json:"arguments"`
}
