package agent

import (
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.MaxSteps != 30 || c.MaxRuns != 20 || c.MaxTokens != 4096 ||
		c.RequeueDelay != 30*time.Second || c.PausedDelay != 5*time.Minute || c.KillSwitch || c.Now == nil {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestWithDefaultsFillsZeroOnly(t *testing.T) {
	c := (Config{MaxSteps: 5}).withDefaults()
	if c.MaxSteps != 5 || c.MaxRuns != 20 || c.MaxTokens != 4096 || c.Now == nil ||
		c.RequeueDelay != 30*time.Second || c.PausedDelay != 5*time.Minute {
		t.Fatalf("got %+v", c)
	}
}

func TestValidateRejectsNegative(t *testing.T) {
	if err := DefaultConfig().validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []Config{
		{MaxSteps: -1}, {MaxRuns: -1}, {MaxTokens: -1}, {RequeueDelay: -1}, {PausedDelay: -1},
	} {
		if err := c.validate(); err == nil {
			t.Errorf("%+v: want error", c)
		}
	}
	if err := (Config{}).validate(); err != nil {
		t.Errorf("zero config must validate: %v", err)
	}
}
