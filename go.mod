module github.com/go-filesystems/xfs

go 1.25.0

require github.com/go-diskimages/qcow2 v0.0.0

require github.com/go-filesystems/interface v0.0.0

replace github.com/go-diskimages/qcow2 => ../../go-diskimages/qcow2

replace github.com/go-filesystems/interface => ../interface
