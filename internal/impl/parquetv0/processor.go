package parquetv0

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/xitongsys/parquet-go-source/buffer"
	"github.com/xitongsys/parquet-go/parquet"
	"github.com/xitongsys/parquet-go/reader"
	"github.com/xitongsys/parquet-go/writer"

	"github.com/benthosdev/benthos/v4/public/service"
)

func parquetProcessorConfig() *service.ConfigSpec {
	return service.NewConfigSpec().
		Deprecated().
		Categories("Parsing").
		Summary("Converts batches of documents to or from [Parquet files](https://parquet.apache.org/documentation/latest/).").
		Description(`
:::warning SCHEDULED FOR REMOVAL
This processor has been deprecated and scheduled for removal. This is due to ongoing concerns with the underlying library used in the implementation. A new library is being investigated and, provided it meets our standards, we will be recreating a more intuitive ` + "`parquet`" + ` processor before eventually removing this one.
:::

### Troubleshooting

This processor is experimental and the error messages that it provides are often vague and unhelpful. An error message of the form ` + "`interface {} is nil, not <value type>`" + ` implies that a field of the given type was expected but not found in the processed message when writing parquet files.

Unfortunately the name of the field will sometimes be missing from the error, in which case it's worth double checking the schema you provided to make sure that there are no typos in the field names, and if that doesn't reveal the issue it can help to mark fields as OPTIONAL in the schema and gradually change them back to REQUIRED until the error returns.

### Defining the Schema

The schema must be specified as a JSON string, containing an object that describes the fields expected at the root of each document. Each field can itself have more fields defined, allowing for nested structures:

` + "```json" + `
{
  "Tag": "name=root, repetitiontype=REQUIRED",
  "Fields": [
    {"Tag": "name=name, inname=NameIn, type=BYTE_ARRAY, convertedtype=UTF8, repetitiontype=REQUIRED"},
    {"Tag": "name=age, inname=Age, type=INT32, repetitiontype=REQUIRED"},
    {"Tag": "name=id, inname=Id, type=INT64, repetitiontype=REQUIRED"},
    {"Tag": "name=weight, inname=Weight, type=FLOAT, repetitiontype=REQUIRED"},
    {
      "Tag": "name=favPokemon, inname=FavPokemon, type=LIST, repetitiontype=OPTIONAL",
      "Fields": [
        {"Tag": "name=name, inname=PokeName, type=BYTE_ARRAY, convertedtype=UTF8, repetitiontype=REQUIRED"},
        {"Tag": "name=coolness, inname=Coolness, type=FLOAT, repetitiontype=REQUIRED"}
      ]
    }
  ]
}
` + "```" + ``).
		Field(service.NewStringAnnotatedEnumField("operator", map[string]string{
			"to_json":   "Expand a file into one or more JSON messages.",
			"from_json": "Compress a batch of JSON documents into a file.",
		}).
			Description("Determines whether the processor converts messages into a parquet file or expands parquet files into messages. Converting into JSON allows subsequent processors and mappings to convert the data into any other format.")).
		Field(service.NewStringEnumField("compression", "uncompressed", "snappy", "gzip", "lz4", "zstd" /*, "lzo", "brotli", "lz4_raw" */).
			Description("The type of compression to use when writing parquet files, this field is ignored when consuming parquet files.").
			Default("snappy")).
		Field(service.NewStringField("schema_file").
			Description("A file path containing a schema used to describe the parquet files being generated or consumed, the format of the schema is a JSON document detailing the tag and fields of documents. The schema can be found at: https://pkg.go.dev/github.com/xitongsys/parquet-go#readme-json. Either a `schema_file` or `schema` field must be specified.").
			Optional().
			Example(`schemas/foo.json`)).
		Field(service.NewStringField("schema").
			Description("A schema used to describe the parquet files being generated or consumed, the format of the schema is a JSON document detailing the tag and fields of documents. The schema can be found at: https://pkg.go.dev/github.com/xitongsys/parquet-go#readme-json. Either a `schema_file` or `schema` field must be specified.").
			Optional().
			Example(`{
  "Tag": "name=root, repetitiontype=REQUIRED",
  "Fields": [
    {"Tag":"name=name,inname=NameIn,type=BYTE_ARRAY,convertedtype=UTF8, repetitiontype=REQUIRED"},
    {"Tag":"name=age,inname=Age,type=INT32,repetitiontype=REQUIRED"}
  ]
}`)).
		Version("3.62.0")
}

func init() {
	err := service.RegisterBatchProcessor(
		"parquet_old", parquetProcessorConfig(),
		func(conf *service.ParsedConfig, mgr *service.Resources) (service.BatchProcessor, error) {
			return newParquetProcessorFromConfig(conf, mgr.Logger())
		})

	if err != nil {
		panic(err)
	}
}

//------------------------------------------------------------------------------

func getCompressionType(str string) (parquet.CompressionCodec, error) {
	switch str {
	case "uncompressed":
		return parquet.CompressionCodec_UNCOMPRESSED, nil
	case "snappy":
		return parquet.CompressionCodec_SNAPPY, nil
	case "gzip":
		return parquet.CompressionCodec_GZIP, nil
	case "lz4":
		return parquet.CompressionCodec_LZ4, nil
	case "zstd":
		return parquet.CompressionCodec_ZSTD, nil
	}
	return parquet.CompressionCodec_UNCOMPRESSED, fmt.Errorf("unknown compression type: %v", str)

}

func newParquetProcessorFromConfig(conf *service.ParsedConfig, logger *service.Logger) (*parquetProcessor, error) {
	operator, err := conf.FieldString("operator")
	if err != nil {
		return nil, err
	}
	var rawSchema string
	if conf.Contains("schema") {
		if rawSchema, err = conf.FieldString("schema"); err != nil {
			return nil, err
		}
	}
	if conf.Contains("schema_file") {
		schemaFile, err := conf.FieldString("schema_file")
		if err != nil {
			return nil, err
		}
		if schemaFile != "" {
			rawSchemaBytes, err := os.ReadFile(schemaFile)
			if err != nil {
				return nil, fmt.Errorf("failed to read schema file: %w", err)
			}
			rawSchema = string(rawSchemaBytes)
		}
	}
	if rawSchema == "" {
		return nil, errors.New("either a raw `schema` or a non-empty `schema_file` must be specified")
	}

	cCodec, err := conf.FieldString("compression")
	if err != nil {
		return nil, err
	}
	return newParquetProcessor(operator, cCodec, rawSchema, logger)
}

type parquetProcessor struct {
	schema   string
	operator func(context.Context, service.MessageBatch) ([]service.MessageBatch, error)
	logger   *service.Logger
	cCodec   parquet.CompressionCodec
}

func newParquetProcessor(operator, compressionCodec, schemaStr string, logger *service.Logger) (*parquetProcessor, error) {
	s := &parquetProcessor{
		schema: schemaStr,
		logger: logger,
	}
	switch operator {
	case "from_json":
		s.operator = s.processBatchWriter
		var err error
		if s.cCodec, err = getCompressionType(compressionCodec); err != nil {
			return nil, err
		}
	case "to_json":
		s.operator = s.processBatchReader
	default:
		return nil, fmt.Errorf("unrecognised operator: %v", operator)
	}
	return s, nil
}

func (s *parquetProcessor) ProcessBatch(ctx context.Context, batch service.MessageBatch) ([]service.MessageBatch, error) {
	return s.operator(ctx, batch)
}

func (s *parquetProcessor) processBatchReader(ctx context.Context, batch service.MessageBatch) ([]service.MessageBatch, error) {
	if len(batch) == 0 {
		return nil, nil
	}

	outBatches := make([]service.MessageBatch, len(batch))
	for i, m := range batch {
		mBytes, err := m.AsBytes()
		if err != nil {
			return nil, fmt.Errorf("failed to read message contents: %w", err)
		}

		buf := buffer.NewBufferFileFromBytes(mBytes)

		pr, err := reader.NewParquetReader(buf, s.schema, 1)
		if err != nil {
			return nil, fmt.Errorf("failed to create parquet reader: %w", err)
		}

		var outBatch service.MessageBatch
		for j := 0; j < int(pr.GetNumRows()); j++ {
			res, err := pr.ReadByNumber(j)
			if err != nil {
				return nil, fmt.Errorf("failed to read parquet row: %w", err)
			}
			for _, v := range res {
				outMsg := m.Copy()
				outMsg.SetStructured(v)
				outBatch = append(outBatch, outMsg)
			}
		}

		pr.ReadStop()
		outBatches[i] = outBatch
	}

	return outBatches, nil
}

func (s *parquetProcessor) processBatchWriter(ctx context.Context, batch service.MessageBatch) ([]service.MessageBatch, error) {
	if len(batch) == 0 {
		return nil, nil
	}

	buf := buffer.NewBufferFile()

	pw, err := writer.NewJSONWriter(s.schema, buf, 1)
	if err != nil {
		return nil, fmt.Errorf("failed to create parquet writer: %w", err)
	}
	pw.CompressionType = s.cCodec

	for _, m := range batch {
		b, err := m.AsBytes()
		if err != nil {
			return nil, fmt.Errorf("failed to parse message as structured: %w", err)
		}
		if err = pw.Write(b); err != nil {
			return nil, fmt.Errorf("failed to write document to parquet file: %w", err)
		}
	}

	if err := pw.WriteStop(); err != nil {
		return nil, fmt.Errorf("failed to close parquet writer: %w", err)
	}

	outMsg := batch[0].Copy()
	outMsg.SetBytes(buf.Bytes())
	return []service.MessageBatch{{outMsg}}, nil
}

func (s *parquetProcessor) Close(ctx context.Context) error {
	return nil
}