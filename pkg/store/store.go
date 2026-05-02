// Package store 定义外部存储客户端封装：Redis / MySQL / Kafka / 对象存储 等。
//
// 各 client 通过接口暴露；具体实现可插拔。
//
// TODO: 各 client 在 step 2+ 加。
package store
