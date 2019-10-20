<?php

namespace App\Magnetico\Entity;

use Doctrine\Common\Collections\ArrayCollection;
use Doctrine\ORM\Mapping as ORM;
use Symfony\Component\Serializer\Annotation as Serializer;

/**
 * @ORM\Table(schema="magneticod", name="torrents", indexes={
 *     @ORM\Index(name="discovered_on_index", columns={"discovered_on"}),
 *     @ORM\Index(name="info_hash_index", columns={"info_hash"})
 * })
 * @ORM\Entity(readOnly=true, repositoryClass="App\Magnetico\Repository\TorrentRepository")
 */
class Torrent
{
    /**
     * @var int
     *
     * @Serializer\Groups({"api_v1_search", "api_v1_show"})
     *
     * @ORM\Column(name="id", type="integer")
     * @ORM\Id
     */
    private $id;

    /**
     * @var resource Resource pointing to info-hash BLOB
     *
     * @Serializer\Groups({"api_v1_search", "api_v1_show"})
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
     * @Serializer\Groups({"api_v1_search", "api_v1_show"})
     *
     * @ORM\Column(name="name", type="text", nullable=false)
     */
    private $name;

    /**
     * @var int Torrent files total size in bytes
     *
     * @Serializer\Groups({"api_v1_search", "api_v1_show"})
     *
     * @ORM\Column(name="total_size", type="bigint", nullable=false)
     */
    private $totalSize;

    /**
     * @var int Torrent discovery timestamp
     *
     * @Serializer\Groups({"api_v1_search", "api_v1_show"})
     *
     * @ORM\Column(name="discovered_on", type="integer", nullable=false)
     */
    private $discoveredOn;

    /**
     * @var File[]|ArrayCollection
     *
     * @Serializer\Groups({"api_v1_show"})
     *
     * @ORM\OneToMany(targetEntity="App\Magnetico\Entity\File", fetch="EXTRA_LAZY", mappedBy="torrent")
     */
    private $files;

    public function getId(): int
    {
        return $this->id;
    }

    /** Returns torrent info hash as HEX string */
    public function getInfoHash(): string
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
