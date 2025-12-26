package consts

const (
	// ScannerChannelBufferMultiplier determines the buffer size of the paths channel
	// relative to the number of workers.
	ScannerChannelBufferMultiplier = 10

	// DBBatchSize is the number of file records to batch before inserting into the database.
	DBBatchSize = 1000
)
