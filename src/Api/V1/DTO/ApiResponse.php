<?php
declare(strict_types=1);

namespace App\Api\V1\DTO;

use App\Api\V1\Enum\StatusEnum;
use Symfony\Component\HttpFoundation\Response;
use Symfony\Component\Serializer\Annotation\Groups;

class ApiResponse
{
    #[Groups(['api'])]
    private int $code;

    /** Status: 'success' (1xx-3xx), 'error' (4xx), 'fail' (5xx) or 'unknown' */
    #[Groups(['api'])]
    private StatusEnum $status;

    /** Used for 'fail' and 'error' */
    #[Groups(['api'])]
    private ?string $message;

    /** Response body. In case of 'error' or 'fail' contains cause or exception name. */
    #[Groups(['api'])]
    private string|object|array|null $data;

    public function __construct($data = null, int $code = Response::HTTP_OK, ?string $message = null, ?StatusEnum $status = null)
    {
        $this->data = $data;
        $this->code = $code;
        $this->message = $message;

        if ($code >= 100 && $code < 300) {
            $this->status = StatusEnum::Success;
        } elseif ($code >= 400 && $code < 500) {
            $this->status = StatusEnum::Error;
        } elseif ($code >= 500 && $code < 600) {
            $this->status = StatusEnum::Fail;
        } else {
            $this->status = StatusEnum::Unknown;
        }
    }

    public function getCode(): int
    {
        return $this->code;
    }

    public function getStatus(): StatusEnum
    {
        return $this->status;
    }

    public function getMessage(): ?string
    {
        return $this->message;
    }

    /** @return array|\object|string|null */
    public function getData()
    {
        return $this->data;
    }
}