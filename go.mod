module github.com/stakater/operator-utils

go 1.14

require (
	github.com/operator-framework/operator-sdk v0.18.1
	github.com/redhat-cop/operator-utils v0.3.4
	k8s.io/api v0.18.8
	k8s.io/apimachinery v0.18.8
	sigs.k8s.io/controller-runtime v0.6.2
)

replace k8s.io/client-go => k8s.io/client-go v0.18.2
