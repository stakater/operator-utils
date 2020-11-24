module github.com/stakater/operator-utils

go 1.15

require (
	github.com/go-logr/logr v0.2.1
	github.com/go-logr/zapr v0.3.0 // indirect
	github.com/google/go-cmp v0.5.3
	github.com/prometheus/client_golang v1.5.1 // indirect
	github.com/stretchr/testify v1.6.1 // indirect
	go.uber.org/zap v1.16.0 // indirect
	golang.org/x/sys v0.0.0-20200625212154-ddb9806d33ae // indirect
	golang.org/x/tools v0.0.0-20200403190813-44a64ad78b9b // indirect
	google.golang.org/protobuf v1.25.0 // indirect
	k8s.io/api v0.19.4
	k8s.io/apiextensions-apiserver v0.18.8 // indirect
	k8s.io/apimachinery v0.19.4
	k8s.io/client-go v12.0.0+incompatible // indirect
	sigs.k8s.io/controller-runtime v0.6.4
)

replace k8s.io/client-go => k8s.io/client-go v0.19.0
