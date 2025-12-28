package consts

const (
	// Version is the current application version
	Version = "0.8.9"

	// ScannerChannelBufferMultiplier determines the buffer size of the paths channel
	// relative to the number of workers.
	ScannerChannelBufferMultiplier = 1000

	// DBBatchSize is the number of file records to batch before inserting into the database.
	DBBatchSize = 1000
)
