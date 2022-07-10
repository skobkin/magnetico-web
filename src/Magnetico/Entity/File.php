<?php
declare(strict_types=1);

namespace App\Magnetico\Entity;

use Doctrine\ORM\Mapping as ORM;
use Symfony\Component\Serializer\Annotation as Serializer;

#[ORM\Entity(readOnly: true)]
#[ORM\Table(schema: 'magneticod', name: 'files')]
#[ORM\Index(name: 'file_info_hash_index', columns: ['torrent_id'])]
class File
{
    #[ORM\Id]
    #[ORM\Column(name: 'id', type: 'integer')]
    private int $id;

    #[ORM\ManyToOne(targetEntity: Torrent::class, inversedBy: 'files')]
    #[ORM\JoinColumn(name: 'torrent_id')]
    private Torrent $torrent;

    /** File size in bytes */
    #[Serializer\Groups(['api_v1_show'])]
    #[ORM\Column(name: 'size', type: 'bigint', nullable: false)]
    private int $size;

    #[Serializer\Groups(['api_v1_show'])]
    #[ORM\Column(name: 'path', type: 'text', nullable: false)]
    private string $path;

    public function getId(): int
    {
        return $this->id;
    }

    public function getTorrent(): Torrent
    {
        return $this->torrent;
    }

    /** Returns file size in bytes */
    public function getSize(): int
    {
        return $this->size;
    }

    /** Returns file path relative to the torrent root directory */
    public function getPath(): string
    {
        return $this->path;
    }
}
