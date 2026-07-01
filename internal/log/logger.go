package log

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Level 日志级别
// 目前按枚举整数实现，可用于后续扩展分级过滤逻辑。
type Level int

const (
	LevelDebug Level = iota // LevelDebug 调试级别，用于开发期详细追踪
	LevelInfo               // LevelInfo 信息级别，默认业务日志
	LevelWarn               // LevelWarn 警告级别，非致命异常
	LevelError              // LevelError 错误级别，影响功能的问题
)

// levelNames 日志级别到文本名称的映射
// 该映射为只读常量映射，供格式化输出时使用。
var levelNames = map[Level]string{
	LevelDebug: "DEBUG",
	LevelInfo:  "INFO",
	LevelWarn:  "WARN",
	LevelError: "ERROR",
}

// Logger 日志器
// 当前实现为简单控制台/IO 日志器，内部通过互斥锁保证并发安全。
type Logger struct {
	mu      sync.Mutex
	writer  io.Writer
	level   Level
	showTime bool
}

// New 创建日志器
// writer 为 nil 时回退到标准输出；showTime 控制是否打印时间前缀。
func New(writer io.Writer, level Level, showTime bool) *Logger {
	if writer == nil {
		writer = os.Stdout
	}
	return &Logger{
		writer:   writer,
		level:    level,
		showTime: showTime,
	}
}

// NewConsole 创建控制台日志器
// 默认使用标准输出、INFO 级别并显示时间。
func NewConsole() *Logger {
	return New(os.Stdout, LevelInfo, true)
}

// SetLevel 设置日志级别
// 低于该级别的日志将不会输出。
func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

// Debug 调试日志
func (l *Logger) Debug(format string, args ...any) {
	l.log(LevelDebug, format, args...)
}

// Info 信息日志
func (l *Logger) Info(format string, args ...any) {
	l.log(LevelInfo, format, args...)
}

// Warn 警告日志
func (l *Logger) Warn(format string, args ...any) {
	l.log(LevelWarn, format, args...)
}

// Error 错误日志
func (l *Logger) Error(format string, args ...any) {
	l.log(LevelError, format, args...)
}

// log 输出日志
// 在日志级别满足要求后格式化并写入底层 writer。
func (l *Logger) log(level Level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if level < l.level {
		return
	}

	prefix := levelNames[level]
	if l.showTime {
		prefix = time.Now().Format("15:04:05") + " " + prefix
	}

	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(l.writer, "[%s] %s\n", prefix, msg)
}

// Writer 返回底层 writer
func (l *Logger) Writer() io.Writer {
	return l.writer
}
