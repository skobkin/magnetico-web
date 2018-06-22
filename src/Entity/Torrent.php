<?php

namespace App\Entity;

use Doctrine\Common\Collections\ArrayCollection;
use Doctrine\ORM\Mapping as ORM;

/**
 * @ORM\Table(name="torrents", indexes={
 *     @ORM\Index(name="discovered_on_index", columns={"discovered_on"}),
 *     @ORM\Index(name="info_hash_index", columns={"info_hash"})
 * })
 * @ORM\Entity(readOnly=true, repositoryClass="App\Repository\TorrentRepository")
 */
class Torrent
{
    /**
     * @var int
     *
     * @ORM\Column(name="id", type="integer")
     * @ORM\Id
     */
    private $id;

    /**
     * @var resource Resource pointing to info-hash BLOB
     *
     * @ORM\Column(name="info_hash", type="blob", nullable=false)
     */
    private $infoHash;

    /**
     * @var string Cached value of self::infoHash in HEX string
     */
    private $infoHashHexCache;

    /**
     * @var string Torrent name
     *
     * @ORM\Column(name="name", type="text", nullable=false)
     */
    private $name;

    /**
     * @var int Torrent files total size in bytes
     *
     * @ORM\Column(name="total_size", type="integer", nullable=false)
     */
    private $totalSize;

    /**
     * @var int Torrent discovery timestamp
     *
     * @ORM\Column(name="discovered_on", type="integer", nullable=false)
     */
    private $discoveredOn;

    /**
     * @var File[]|ArrayCollection
     *
     * @ORM\OneToMany(targetEntity="App\Entity\File", fetch="EXTRA_LAZY", mappedBy="torrent")
     */
    private $files;

    public function getId(): int
    {
        return $this->id;
    }

    /**
     * Returns torrent info hash BLOB resource
     *
     * @return resource
     */
    public function getInfoHash()
    {
        return $this->infoHash;
    }

    /** Returns torrent info hash as HEX string */
    public function getInfoHashAsHex(): string
    {
        if (null === $this->infoHashHexCache) {
            $this->infoHashHexCache = bin2hex(stream_get_contents($this->infoHash));
            rewind($this->infoHash);
        }

        return $this->infoHashHexCache;
    }

    public function getName(): string
    {
        return $this->name;
    }

    /** Returns torrent files total size in bytes */
    public function getTotalSize(): int
    {
        return $this->totalSize;
    }

    /** Returns torrent discovery timestamp */
    public function getDiscoveredOn(): int
    {
        return $this->discoveredOn;
    }

    /**
     * @return File[]|ArrayCollection
     */
    public function getFiles(): iterable
    {
        return $this->files;
    }
}
