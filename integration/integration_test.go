package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pascaldekloe/mqtt"
)

func TestRace(t *testing.T) {
	hosts := strings.Fields(os.Getenv("MQTT_HOSTS"))
	if len(hosts) == 0 {
		hosts = append(hosts, "localhost")
	}

	for i := range hosts {
		t.Run(hosts[i], func(t *testing.T) {
			host := hosts[i]
			t.Parallel()
			t.Run("at-most-once", func(t *testing.T) {
				t.Parallel()
				race(t, host, 0)
			})
			t.Run("at-least-once", func(t *testing.T) {
				t.Parallel()
				race(t, host, 1)
			})
			t.Run("exactly-once", func(t *testing.T) {
				t.Parallel()
				race(t, host, 2)
			})
		})
	}
}

func race(t *testing.T, host string, deliveryLevel int) {
	const testN = 100
	done := make(chan struct{}) // closed once testN messages received
	testMessage := []byte("Hello World!")
	testTopic := fmt.Sprintf("test/race-%d", deliveryLevel)

	client := mqtt.NewClient(&mqtt.ClientConfig{
		Connecter: mqtt.UnsecuredConnecter("tcp", net.JoinHostPort(host, "1883")),

		SessionConfig: mqtt.NewVolatileSessionConfig(t.Name()),
		BufSize:        1024,
		WireTimeout:    time.Second,
		AtLeastOnceMax: testN,
		ExactlyOnceMax: testN,
	})

	var wg sync.WaitGroup
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := client.Disconnect(ctx.Done()); err != nil {
			t.Error(err)
		}
		wg.Wait()
	})

	wg.Add(1)
	go func() {
		defer wg.Done()

		// collect until closed
		var gotN int
		for {
			message, topic, err := client.ReadSlices()
			switch {
			case err == nil:
				if !bytes.Equal(message, testMessage) || string(topic) != testTopic {
					t.Errorf("got message %q @ %q, want %q @ %q", message, topic, testMessage, testTopic)
				}
				gotN++
				if gotN == testN {
					close(done)
				}

			case errors.Is(err, mqtt.ErrClosed):
				if gotN != testN {
					t.Errorf("got %d messages, want %d", gotN, testN)
				}
				return

			default:
				t.Log("read error:", err)
				time.Sleep(time.Second/8)
				continue
			}
		}
	}()

	err := client.Subscribe(nil, testTopic)
	if err != nil {
		t.Fatal("subscribe error: ", err)
	}

	// install contenders
	launch := make(chan struct{})
	wg.Add(testN)
	for i := 0; i < testN; i++ {
		go func() {
			defer wg.Done()
			ack := make(chan error, 1)
			<-launch
			var err error
			switch deliveryLevel {
			case 0:
				err = client.Publish(testMessage, testTopic)
				close(ack)
			case 1:
				err = client.PublishAtLeastOnce(testMessage, testTopic, ack)
			case 2:
				err = client.PublishExactlyOnce(testMessage, testTopic, ack)
			}
			if err != nil {
				t.Error("publish error:", err)
				return
			}
			for err := range ack {
				if errors.Is(err, mqtt.ErrClosed) {
					break
				}
				t.Error("publish error:", err)
			}
		}()
	}

	time.Sleep(time.Second / 4) // await subscription & contenders
	close(launch)
	select {
	case <-done:
		break
	case <-time.After(4 * time.Second):
		t.Fatal("timeout awaiting message reception")
	}
}
