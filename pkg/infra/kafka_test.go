package infra

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNewKafkaProducer_RejectsEmptyBrokers(t *testing.T) {
	if _, err := NewKafkaProducer(KafkaConfig{}); err == nil {
		t.Fatal("want error for empty brokers")
	}
	if _, err := NewKafkaProducer(KafkaConfig{Brokers: []string{}}); err == nil {
		t.Fatal("want error for empty brokers slice")
	}
}

func TestKafkaProducer_WriteRejectsEmptyTopic(t *testing.T) {
	p, err := NewKafkaProducer(KafkaConfig{Brokers: []string{"localhost:9092"}})
	if err != nil {
		t.Fatalf("NewKafkaProducer: %v", err)
	}
	defer func() { _ = p.Close() }()
	if err := p.Write(context.Background(), "", []byte("k"), []byte("v")); err == nil {
		t.Fatal("want error for empty topic")
	}
}

func TestKafkaProducer_CloseNeverUsed(t *testing.T) {
	// kafka.Writer 是 lazy connect；从没 Write 过的 Close 应该是 no-op 不报错。
	p, err := NewKafkaProducer(KafkaConfig{Brokers: []string{"localhost:9092"}})
	if err != nil {
		t.Fatalf("NewKafkaProducer: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestKafkaProducer_WriteIntegration 真 broker 集成测试，
// gate 在 KAFKA_BROKERS 环境变量后面；CI 默认跳过。
//
// 跑法：
//
//	docker run -p 9092:9092 apache/kafka:latest
//	KAFKA_BROKERS=localhost:9092 KAFKA_TOPIC=ai-gateway-test go test ./pkg/infra/...
func TestKafkaProducer_WriteIntegration(t *testing.T) {
	brokers := os.Getenv("KAFKA_BROKERS")
	if brokers == "" {
		t.Skip("KAFKA_BROKERS not set; skipping integration test")
	}
	topic := os.Getenv("KAFKA_TOPIC")
	if topic == "" {
		topic = "ai-gateway-test"
	}

	p, err := NewKafkaProducer(KafkaConfig{Brokers: strings.Split(brokers, ",")})
	if err != nil {
		t.Fatalf("NewKafkaProducer: %v", err)
	}
	defer func() { _ = p.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.Write(ctx, topic, []byte("test-key"), []byte(`{"hello":"world"}`)); err != nil {
		t.Errorf("Write: %v", err)
	}
}
