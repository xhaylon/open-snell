/*
 * This file is part of open-snell.
 * open-snell is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 * open-snell is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 * You should have received a copy of the GNU General Public License
 * along with open-snell.  If not, see <https://www.gnu.org/licenses/>.
 */

package aead

import (
    "bytes"
    "crypto/cipher"
    "crypto/rand"
    "errors"
    "io"
    "net"
)

const payloadSizeMask = 0x3FFF // 16*1024 - 1

var ErrZeroChunk = errors.New("Snell ZERO_CHUNK occurred")

type writer struct {
    io.Writer
    cipher.AEAD
    nonce []byte
    buf   []byte
}

func NewWriter(w io.Writer, aead cipher.AEAD) io.Writer { return newWriter(w, aead) }

func newWriter(w io.Writer, aead cipher.AEAD) *writer {
    return &writer{
        Writer: w,
        AEAD:   aead,
        buf:    make([]byte, 2+aead.Overhead()+payloadSizeMask+aead.Overhead()),
        nonce:  make([]byte, aead.NonceSize()),
    }
}

func (w *writer) Write(b []byte) (int, error) {
    if len(b) == 0 { // zero chunk
        buf := w.buf
        buf = buf[:2+w.Overhead()]

        buf[0], buf[1] = 0, 0
        w.Seal(buf[:0], w.nonce, buf[:2], nil)
        increment(w.nonce)

        _, err := w.Writer.Write(buf)
        return 0, err
    }

    n, err := w.ReadFrom(bytes.NewBuffer(b))
    return int(n), err
}

func (w *writer) ReadFrom(r io.Reader) (n int64, err error) {
    for {
        buf := w.buf
        payloadBuf := buf[2+w.Overhead() : 2+w.Overhead()+payloadSizeMask]
        nr, er := r.Read(payloadBuf)

        if nr > 0 {
            n += int64(nr)
            buf = buf[:2+w.Overhead()+nr+w.Overhead()]
            payloadBuf = payloadBuf[:nr]
            buf[0], buf[1] = byte(nr>>8), byte(nr) // big-endian payload size
            w.Seal(buf[:0], w.nonce, buf[:2], nil)
            increment(w.nonce)

            w.Seal(payloadBuf[:0], w.nonce, payloadBuf, nil)
            increment(w.nonce)

            _, ew := w.Writer.Write(buf)
            if ew != nil {
                err = ew
                break
            }
        }

        if er != nil {
            if er != io.EOF { // ignore EOF as per io.ReaderFrom contract
                err = er
            }
            break
        }
    }

    return n, err
}

type reader struct {
    io.Reader
    cipher.AEAD
    nonce    []byte
    buf      []byte
    leftover []byte
    fallback cipher.AEAD
}

// NewReader wraps an io.Reader with AEAD decryption.
func NewReader(r io.Reader, aead cipher.AEAD) io.Reader { return newReader(r, aead, nil) }

func NewReaderWithFallback(r io.Reader, aead, fallback cipher.AEAD) io.Reader {
    return newReader(r, aead, fallback)
}

func newReader(r io.Reader, aead cipher.AEAD, fallback cipher.AEAD) *reader {
    return &reader{
        Reader:   r,
        AEAD:     aead,
        buf:      make([]byte, payloadSizeMask+aead.Overhead()),
        nonce:    make([]byte, aead.NonceSize()),
        fallback: fallback,
    }
}

// read and decrypt a record into the internal buffer. Return decrypted payload length and any error encountered.
func (r *reader) read() (int, error) {
    // decrypt payload size
    buf := r.buf[:2+r.Overhead()]
    _, err := io.ReadFull(r.Reader, buf)
    if err != nil {
        return 0, err
    }


    if r.fallback != nil {
        tbuf := make([]byte, len(buf))
        copy(tbuf, buf)
        _, err = r.Open(buf[:0], r.nonce, tbuf, nil)
        if err != nil {
            r.AEAD = r.fallback
            _, err = r.Open(buf[:0], r.nonce, tbuf, nil)
        }
        r.fallback = nil
    } else {
        _, err = r.Open(buf[:0], r.nonce, buf, nil)
    }
    increment(r.nonce)
    if err != nil {
        return 0, err
    }

    size := (int(buf[0])<<8 + int(buf[1])) & payloadSizeMask

    if size == 0 {
        return 0, ErrZeroChunk
    }

    // decrypt payload
    buf = r.buf[:size+r.Overhead()]
    _, err = io.ReadFull(r.Reader, buf)
    if err != nil {
        return 0, err
    }

    _, err = r.Open(buf[:0], r.nonce, buf, nil)
    increment(r.nonce)
    if err != nil {
        return 0, err
    }

    return size, nil
}

// Read reads from the embedded io.Reader, decrypts and writes to b.
func (r *reader) Read(b []byte) (int, error) {
    // copy decrypted bytes (if any) from previous record first
    if len(r.leftover) > 0 {
        n := copy(b, r.leftover)
        r.leftover = r.leftover[n:]
        return n, nil
    }

    n, err := r.read()
    m := copy(b, r.buf[:n])
    if m < n { // insufficient len(b), keep leftover for next read
        r.leftover = r.buf[m:n]
    }
    return m, err
}

// WriteTo reads from the embedded io.Reader, decrypts and writes to w until
// there's no more data to write or when an error occurs. Return number of
// bytes written to w and any error encountered.
func (r *reader) WriteTo(w io.Writer) (n int64, err error) {
    // write decrypted bytes left over from previous record
    for len(r.leftover) > 0 {
        nw, ew := w.Write(r.leftover)
        r.leftover = r.leftover[nw:]
        n += int64(nw)
        if ew != nil {
            return n, ew
        }
    }

    for {
        nr, er := r.read()
        if nr > 0 {
            nw, ew := w.Write(r.buf[:nr])
            n += int64(nw)

            if ew != nil {
                err = ew
                break
            }
        }

        if er != nil {
            if er != io.EOF { // ignore EOF as per io.Copy contract (using src.WriteTo shortcut)
                err = er
            }
            break
        }
    }

    return n, err
}

// increment little-endian encoded unsigned integer b. Wrap around on overflow.
func increment(b []byte) {
    for i := range b {
        b[i]++
        if b[i] != 0 {
            return
        }
    }
}

type streamConn struct {
    net.Conn
    Cipher
    r *reader
    w *writer
    fallback Cipher
}

func (c *streamConn) initReader() error {
    salt := make([]byte, c.SaltSize())
    if _, err := io.ReadFull(c.Conn, salt); err != nil {
        return err
    }
    aead, err := c.Decrypter(salt)
    if err != nil {
        return err
    }

    var fallback cipher.AEAD = nil
    if c.fallback != nil {
        fallback, _ = c.fallback.Decrypter(salt)
    }

    c.r = newReader(c.Conn, aead, fallback)
    return nil
}

func (c *streamConn) Read(b []byte) (int, error) {
    if c.r == nil {
        if err := c.initReader(); err != nil {
            return 0, err
        }
        n, err := c.r.Read(b)
        if c.fallback != nil && c.r.fallback == nil { // cipher switched
            c.Cipher = c.fallback
            c.fallback = nil
        }
        return n, err
    }
    return c.r.Read(b)
}

func (c *streamConn) WriteTo(w io.Writer) (int64, error) {
    if c.r == nil {
        if err := c.initReader(); err != nil {
            return 0, err
        }
        n, err := c.r.WriteTo(w)
        if c.fallback != nil && c.r.fallback == nil { // cipher switched
            c.Cipher = c.fallback
            c.fallback = nil
        }
        return n, err
    }
    return c.r.WriteTo(w)
}

func (c *streamConn) initWriter() error {
    salt := make([]byte, c.SaltSize())
    if _, err := io.ReadFull(rand.Reader, salt); err != nil {
        return err
    }
    aead, err := c.Encrypter(salt)
    if err != nil {
        return err
    }
    _, err = c.Conn.Write(salt)
    if err != nil {
        return err
    }
    c.w = newWriter(c.Conn, aead)
    return nil
}

func (c *streamConn) Write(b []byte) (int, error) {
    if c.w == nil {
        if err := c.initWriter(); err != nil {
            return 0, err
        }
    }
    return c.w.Write(b)
}

func (c *streamConn) ReadFrom(r io.Reader) (int64, error) {
    if c.w == nil {
        if err := c.initWriter(); err != nil {
            return 0, err
        }
    }
    return c.w.ReadFrom(r)
}

// NewConn wraps a stream-oriented net.Conn with cipher.
func NewConn(c net.Conn, ciph Cipher) net.Conn { return &streamConn{Conn: c, Cipher: ciph} }

func NewConnWithFallback(c net.Conn, ciph, fallback Cipher) net.Conn {
    return &streamConn{
        Conn: c,
        Cipher: ciph,
        fallback: fallback,
    }
}
