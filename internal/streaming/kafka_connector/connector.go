package kafka_connector

import (
	"context"
	"strings"

	"github.com/cloudevents/sdk-go/v2/event"
	"github.com/confluentinc/confluent-kafka-go/v2/kafka"
	"github.com/confluentinc/confluent-kafka-go/v2/schemaregistry"
	"github.com/thmeitz/ksqldb-go"
	"github.com/thmeitz/ksqldb-go/net"
	"golang.org/x/exp/slog"

	"github.com/openmeterio/openmeter/internal/models"
	. "github.com/openmeterio/openmeter/internal/streaming"
)

type KafkaConnector struct {
	config         *KafkaConnectorConfig
	KafkaProducer  *kafka.Producer
	KsqlDBClient   *ksqldb.KsqldbClient
	SchemaRegistry *schemaregistry.Client
	Schema         *Schema
}

type KafkaConnectorConfig struct {
	Kafka          *kafka.ConfigMap
	KsqlDB         *net.Options
	SchemaRegistry *schemaregistry.Config
	EventsTopic    string
	Partitions     int
}

func NewKafkaConnector(config *KafkaConnectorConfig) (Connector, error) {
	ksqldbClient, err := ksqldb.NewClientWithOptions(*config.KsqlDB)
	if err != nil {
		return nil, err
	}

	i, err := ksqldbClient.GetServerInfo()
	if err != nil {
		return nil, err
	}

	slog.Info(
		"connected to ksqlDB",
		"cluster", i.KafkaClusterID,
		"service", i.KsqlServiceID,
		"version", i.Version,
		"status", i.ServerStatus,
	)

	schemaRegistry, err := schemaregistry.NewClient(config.SchemaRegistry)
	if err != nil {
		slog.Error("Schema Registry failed to create client", "error", err)
		return nil, err
	}

	schema, err := NewSchema(SchemaConfig{
		SchemaRegistry: schemaRegistry,
		EventsTopic:    config.EventsTopic,
	})
	if err != nil {
		slog.Error("Schema failed to initialize", "error", err)
		return nil, err
	}

	cloudEventsStreamQuery, err := Execute(cloudEventsStreamQueryTemplate, cloudEventsStreamQueryData{
		Topic:         config.EventsTopic,
		Partitions:    int(config.Partitions),
		KeySchemaId:   int(schema.EventKeySerializer.Conf.UseSchemaID),
		ValueSchemaId: int(schema.EventValueSerializer.Conf.UseSchemaID),
	})
	if err != nil {
		slog.Error("ksqlDB failed to build event stream query", "error", err)
		return nil, err
	}
	slog.Info("ksqlDB create event stream query", "query", cloudEventsStreamQuery)

	detectedEventsTableQuery, err := Execute(detectedEventsTableQueryTemplate, detectedEventsTableQueryData{
		Retention:  32,
		Partitions: int(config.Partitions),
	})
	if err != nil {
		slog.Error("ksqlDB failed to build detected table query", "error", err)
		return nil, err
	}
	slog.Info("ksqlDB create detected table query", "query", detectedEventsTableQuery)

	detectedEventsStreamQuery, err := Execute(detectedEventsStreamQueryTemplate, detectedEventsStreamQueryData{})
	if err != nil {
		slog.Error("ksqlDB failed to build detected stream query", "error", err)
		return nil, err
	}
	slog.Info("ksqlDB create detected stream query", "query", detectedEventsStreamQuery)

	resp, err := ksqldbClient.Execute(ksqldb.ExecOptions{
		KSql: cloudEventsStreamQuery,
	})
	if err != nil {
		slog.Error("ksqlDB failed to create event stream", "error", err)
		return nil, err
	}
	slog.Debug("ksqlDB create event stream response", "response", resp)

	resp, err = ksqldbClient.Execute(ksqldb.ExecOptions{
		KSql: detectedEventsTableQuery,
	})
	if err != nil {
		slog.Error("ksqlDB failed to create detected table", "error", err)
		return nil, err
	}
	slog.Debug("ksqlDB create detected table response", "response", resp)

	resp, err = ksqldbClient.Execute(ksqldb.ExecOptions{
		KSql: detectedEventsStreamQuery,
	})
	if err != nil {
		slog.Error("ksqlDB failed to create detected stream", "error", err)
		return nil, err
	}
	slog.Debug("ksqlDB create detected stream response", "response", resp)

	// Kafka Producer
	producer, err := kafka.NewProducer(config.Kafka)
	if err != nil {
		return nil, err
	}

	slog.Info("connected to Kafka")

	// TODO: move to main
	go func() {
		for e := range producer.Events() {
			switch ev := e.(type) {
			case *kafka.Message:
				// The message delivery report, indicating success or
				// permanent failure after retries have been exhausted.
				// Application level retries won't help since the client
				// is already configured to do that.
				m := ev
				if m.TopicPartition.Error != nil {
					slog.Error("kafka delivery failed", "error", m.TopicPartition.Error)
				} else {
					slog.Debug("kafka message delivered", "topic", *m.TopicPartition.Topic, "partition", m.TopicPartition.Partition, "offset", m.TopicPartition.Offset)
				}
			case kafka.Error:
				// Generic client instance-level errors, such as
				// broker connection failures, authentication issues, etc.
				//
				// These errors should generally be considered informational
				// as the underlying client will automatically try to
				// recover from any errors encountered, the application
				// does not need to take action on them.
				slog.Error("kafka error", "error", ev)
			}
		}
	}()

	connector := &KafkaConnector{
		config:         config,
		KafkaProducer:  producer,
		KsqlDBClient:   &ksqldbClient,
		SchemaRegistry: &schemaRegistry,
		Schema:         schema,
	}

	return connector, nil
}

func (c *KafkaConnector) Init(meter *models.Meter) error {
	queryData := meterTableQueryData{
		Meter:           meter,
		WindowRetention: "36500 DAYS",
		Partitions:      c.config.Partitions,
	}

	err := c.MeterAssert(queryData)
	if err != nil {
		return err
	}

	q, err := GetTableQuery(queryData)
	if err != nil {
		slog.Error("failed to get ksqlDB table", "meter", meter, "error", err)
		return err
	}
	slog.Debug("ksqlDB create table query", "query", q)

	resp, err := c.KsqlDBClient.Execute(ksqldb.ExecOptions{
		KSql: q,
	})
	if err != nil {
		slog.Error("failed to create ksqlDB table", "meter", meter, "query", q, "error", err)
		return err
	}
	slog.Info("ksqlDB response", "response", resp)

	return nil
}

// MeterAssert ensures meter table immutability by checking that existing meter table is the same as new
func (c *KafkaConnector) MeterAssert(data meterTableQueryData) error {
	q, err := GetTableDescribeQuery(data.Meter)
	if err != nil {
		slog.Error("failed to get ksqlDB table", "meter", data.Meter, "error", err)
		return err
	}

	resp, err := c.KsqlDBClient.Execute(ksqldb.ExecOptions{
		KSql: q,
	})
	if err != nil {
		// It's not an issue if the table doesn't exist yet
		// If the table we want to describe does not exist yet ksqldb returns a 40001 error code (bad statement)
		// which is not specific enough to check here.
		if strings.HasPrefix(err.Error(), "Could not find") {
			return nil
		}

		slog.Error("failed to describe ksqlDB table", "meter", data.Meter, "query", q, "error", err)
		return err
	}

	sourceDescription := (*resp)[0]

	if len(sourceDescription.SourceDescription.WriteQueries) > 0 {
		slog.Info("ksqlDB meter assert", "exists", true)

		query := sourceDescription.SourceDescription.WriteQueries[0].QueryString

		err = MeterQueryAssert(query, data)
		if err != nil {
			return err
		}

		slog.Info("ksqlDB meter assert", "equals", true)
	} else {
		slog.Info("ksqlDB meter assert", "exists", false)
	}

	return nil
}

func (c *KafkaConnector) Close() error {
	if c.KafkaProducer != nil {
		c.KafkaProducer.Flush(30 * 1000)
		c.KafkaProducer.Close()
	}
	if c.KsqlDBClient != nil {
		c.KsqlDBClient.Close()
	}
	return nil
}

func (c *KafkaConnector) Publish(event event.Event) error {
	key, err := c.Schema.EventKeySerializer.Serialize(c.config.EventsTopic, event.Subject())
	if err != nil {
		slog.Error("failed to serialize event key", "error", err)
		return err
	}

	ce := ToCloudEventsKafkaPayload(event)
	value, err := c.Schema.EventValueSerializer.Serialize(c.config.EventsTopic, &ce)
	if err != nil {
		slog.Error("failed to serialize event value", "error", err)
		return err
	}

	err = c.KafkaProducer.Produce(&kafka.Message{
		TopicPartition: kafka.TopicPartition{Topic: &c.config.EventsTopic, Partition: kafka.PartitionAny},
		Timestamp:      event.Time(),
		Headers: []kafka.Header{
			{Key: "specversion", Value: []byte(event.SpecVersion())},
		},
		Key:   key,
		Value: value,
	}, nil)

	return err
}

func (c *KafkaConnector) GetValues(meter *models.Meter, params *GetValuesParams) ([]*models.MeterValue, error) {
	q, err := GetTableValuesQuery(meter, params)
	if err != nil {
		return nil, err
	}

	header, payload, err := c.KsqlDBClient.Pull(context.TODO(), ksqldb.QueryOptions{
		Sql: q,
	})
	if err != nil {
		return nil, err
	}

	slog.Debug("ksqlDB response", "header", header, "payload", payload)
	values, err := NewMeterValues(header, payload)
	if err != nil {
		return nil, err
	}

	return meter.AggregateMeterValues(values, params.WindowSize)
}
