module github.com/BRUHItsABunny/gOkHttp-download

go 1.19

replace (
	github.com/cornelk/hashmap v1.0.8 => github.com/BRUHItsABunny/hashmap v0.0.0-20221125164545-8b59f13d589a
	go.uber.org/atomic v1.10.0 => github.com/BRUHItsABunny/atomic v0.0.0-20221125214309-9e798cd18888
)

require (
	github.com/BRUHItsABunny/crypto-utils v0.0.5
	github.com/BRUHItsABunny/gOkHttp v0.3.0
	github.com/cornelk/hashmap v1.0.8
	github.com/dustin/go-humanize v1.0.1
	github.com/etherlabsio/go-m3u8 v1.0.0
	github.com/joho/godotenv v1.5.1
	github.com/yapingcat/gomedia v0.0.0-20240906162731-17feea57090c
	go.uber.org/atomic v1.10.0
	golang.org/x/sync v0.1.0
)

require (
	github.com/OneOfOne/xxhash v1.2.8 // indirect
	golang.org/x/net v0.7.0 // indirect
	golang.org/x/text v0.7.0 // indirect
)
