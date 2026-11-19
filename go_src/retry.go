package main

// Повторы при сетевых сбоях портала (обрыв соединения, таймаут, 5xx).
// Портал fedsfm.ru периодически рвёт TLS-соединение (connection reset by
// peer) — одна такая ошибка не должна превращать прогон в «ошибочный».

import (
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"syscall"
	"time"
)

// errPartialRun — прогон прошёл, но часть разделов не обработана.
// Сводное письмо о таком прогоне runOnce уже отправил сам, поэтому
// runForever по этой ошибке второе письмо («ошибка запуска») не шлёт.
var errPartialRun = errors.New("не удалось обработать разделы")

// isTransient: похоже ли на временный сетевой сбой, который стоит повторить.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	// Ошибки клиента местами обёрнуты через %v, поэтому дублируем по тексту.
	s := strings.ToLower(err.Error())
	for _, m := range []string{
		"connection reset", "connection refused", "broken pipe", "timeout",
		"eof", "tls handshake", "no such host", "server misbehaving",
		"статус 500", "статус 502", "статус 503", "статус 504",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// retryDelay — пауза перед повтором: 5с, 10с, 20с… (как в withRetry).
var retryDelay = func(attempt int) time.Duration {
	return 5 * time.Second << (attempt - 1)
}

// retryWait решает, повторять ли вызов после attempt-й попытки.
// Если ошибка временная и попытки остались — пишет в лог, ждёт и
// возвращает true. Использование:
//
//	x, err := call()
//	for a := 1; retryWait(err, a, cfg.Attempts, "что делаем"); a++ {
//		x, err = call()
//	}
func retryWait(err error, attempt, attempts int, what string) bool {
	if err == nil || attempt >= attempts || !isTransient(err) {
		return false
	}
	d := retryDelay(attempt)
	log.Printf("%s: попытка %d/%d — сетевая ошибка: %v (ждём %s)", what, attempt, attempts, err, d)
	time.Sleep(d)
	return true
}
